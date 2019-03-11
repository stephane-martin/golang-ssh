// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ssh is a helper for working with ssh in go.  The client implementation
// is a modified version of `docker/machine/libmachine/ssh/client.go` and only
// uses golang's native ssh client. It has also been improved to resize the tty
// accordingly. The key functions are meant to be used by either client or server
// and will generate/store keys if not found.
package ssh

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/moby/moby/pkg/term"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

// Client is a relic interface that both native and external client matched
type Client interface {
	// Output returns the output of the command run on the remote host.
	Output(command string) (string, error)

	// Shell requests a shell from the remote. If an arg is passed, it tries to
	// exec them on the server.
	Shell(args ...string) error

	// Start starts the specified command without waiting for it to finish. You
	// have to call the Wait function for that.
	//
	// The first two io.ReadCloser are the standard output and the standard
	// error of the executing command respectively. The returned error follows
	// the same logic as in the exec.Cmd.Start function.
	Start(command string) (io.ReadCloser, io.ReadCloser, error)

	// Wait waits for the command started by the Start function to exit. The
	// returned error follows the same logic as in the exec.Cmd.Wait function.
	Wait() error
}

// NativeClient is the structure for native client use
type NativeClient struct {
	Config        ssh.ClientConfig // Config defines the golang ssh client config
	Hostname      string           // Hostname is the host to connect to
	Port          int              // Port is the port to connect to
	ClientVersion string           // ClientVersion is the version string to send to the server when identifying
	DialRetry     int              // number of dial retries
	openSession   *ssh.Session
	openClient    *ssh.Client
}

// Config is used to create new client.
type Config struct {
	User      string              // username to connect as, required
	Host      string              // hostname to connect to, required
	Version   string              // ssh client version, "SSH-2.0-Go" by default
	Port      int                 // port to connect to, 22 by default
	Auth      []ssh.AuthMethod    // authentication methods to use
	Timeout   time.Duration       // connect timeout, 30s by default
	DialRetry int                 // number of dial retries, 0 (no retries) by default
	HostKey   ssh.HostKeyCallback // callback for verifying server keys, ssh.InsecureIgnoreHostKey by default
}

func (cfg *Config) version() string {
	if cfg.Version != "" {
		return cfg.Version
	}
	return "SSH-2.0-Go"
}

func (cfg *Config) port() int {
	if cfg.Port != 0 {
		return cfg.Port
	}
	return 22
}

func (cfg *Config) timeout() time.Duration {
	if cfg.Timeout != 0 {
		return cfg.Timeout
	}
	return 15 * time.Second
}

func (cfg *Config) hostKey() ssh.HostKeyCallback {
	if cfg.HostKey != nil {
		return cfg.HostKey
	}
	return ssh.InsecureIgnoreHostKey()
}

// NewClient creates a new Client using the golang ssh library.
func NewClient(cfg *Config) (Client, error) {
	config, err := NewNativeConfig(cfg.User, cfg.version(), cfg.hostKey(), cfg.Auth...)
	if err != nil {
		return nil, fmt.Errorf("Error getting config for native Go SSH: %s", err)
	}
	config.Timeout = cfg.timeout()

	return &NativeClient{
		Config:        config,
		Hostname:      cfg.Host,
		Port:          cfg.port(),
		ClientVersion: cfg.version(),
		DialRetry:     cfg.DialRetry,
	}, nil
}

// NewNativeClient creates a new Client using the golang ssh library
func NewNativeClient(user, host, clientVersion string, port int, hostKeyCallback ssh.HostKeyCallback, auth ...ssh.AuthMethod) (Client, error) {
	if clientVersion == "" {
		clientVersion = "SSH-2.0-Go"
	}

	config, err := NewNativeConfig(user, clientVersion, hostKeyCallback, auth...)
	if err != nil {
		return nil, fmt.Errorf("Error getting config for native Go SSH: %s", err)
	}

	return &NativeClient{
		Config:        config,
		Hostname:      host,
		Port:          port,
		ClientVersion: clientVersion,
	}, nil
}

// NewNativeConfig returns a golang ssh client config struct for use by the NativeClient
func NewNativeConfig(user, clientVersion string, hostKeyCallback ssh.HostKeyCallback, auth ...ssh.AuthMethod) (ssh.ClientConfig, error) {
	if hostKeyCallback == nil {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}

	return ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		ClientVersion:   clientVersion,
		HostKeyCallback: hostKeyCallback,
	}, nil
}

func (client *NativeClient) addr() string {
	return fmt.Sprintf("%s:%d", client.Hostname, client.Port)
}

func (client *NativeClient) newSession() (*ssh.Session, *ssh.Client, error) {
	var conn *ssh.Client
	var err error
	for i := client.DialRetry + 1; i > 0; i-- {
		conn, err = ssh.Dial("tcp", client.addr(), &client.Config)
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second) // backoff?
	}
	if err != nil {
		return nil, nil, fmt.Errorf("Error attempting SSH client dial: %s", err)
	}
	session, err := conn.NewSession()
	if err != nil {
		return nil, nil, nonil(err, conn.Close())
	}
	return session, conn, nil
}

// Output returns the output of the command run on the remote host.
func (client *NativeClient) Output(command string) (string, error) {
	session, conn, err := client.newSession()
	if err != nil {
		return "", err
	}

	output, err := session.CombinedOutput(command)
	_, _ = session.Close(), conn.Close()
	return string(bytes.TrimSpace(output)), wrapError(err)
}

// Output returns the output of the command run on the remote host as well as a pty.
func (client *NativeClient) OutputWithPty(command string) (string, error) {
	session, conn, err := client.newSession()
	if err != nil {
		return "", nil
	}
	defer session.Close()
	defer conn.Close()

	fd := int(os.Stdin.Fd())

	termWidth, termHeight, err := terminal.GetSize(fd)
	if err != nil {
		return "", err
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	// request tty -- fixes error with hosts that use
	// "Defaults requiretty" in /etc/sudoers - I'm looking at you RedHat
	if err := session.RequestPty("xterm", termHeight, termWidth, modes); err != nil {
		return "", err
	}

	output, err := session.CombinedOutput(command)

	return string(bytes.TrimSpace(output)), wrapError(err)
}

// Start starts the specified command without waiting for it to finish. You
// have to call the Wait function for that.
func (client *NativeClient) Start(command string) (io.ReadCloser, io.ReadCloser, error) {
	session, conn, err := client.newSession()
	if err != nil {
		return nil, nil, err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, nil, nonil(err, session.Close(), conn.Close())
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, nil, nonil(err, session.Close(), conn.Close())
	}
	if err := session.Start(command); err != nil {
		return nil, nil, nonil(err, session.Close(), conn.Close())
	}
	client.openSession = session
	client.openClient = conn
	return ioutil.NopCloser(stdout), ioutil.NopCloser(stderr), nil
}

// Wait waits for the command started by the Start function to exit. The
// returned error follows the same logic as in the exec.Cmd.Wait function.
func (client *NativeClient) Wait() error {
	err := client.openSession.Wait()
	_, _ = client.openSession.Close(), client.openClient.Close()
	client.openSession, client.openClient = nil, nil
	return err
}

// Shell requests a shell from the remote. If an arg is passed, it tries to
// exec them on the server.
func (client *NativeClient) Shell(args ...string) error {
	var (
		termWidth, termHeight = 80, 24
	)
	conn, err := ssh.Dial("tcp", client.addr(), &client.Config)
	if err != nil {
		return err
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return err
	}

	defer session.Close()

	session.Stdout = os.Stdout
	session.Stderr = os.Stderr
	session.Stdin = os.Stdin

	modes := ssh.TerminalModes{
		ssh.ECHO: 1,
	}

	fd := os.Stdin.Fd()

	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}

		defer term.RestoreTerminal(fd, oldState)

		winsize, err := term.GetWinsize(fd)
		if err == nil {
			termWidth = int(winsize.Width)
			termHeight = int(winsize.Height)
		}
	}

	if err := session.RequestPty("xterm", termHeight, termWidth, modes); err != nil {
		return err
	}

	if len(args) != 0 {
		session.Run(strings.Join(args, " "))
		return nil
	}

	if err := session.Shell(); err != nil {
		return err
	}
	// monitor for sigwinch
	go monWinCh(session, os.Stdout.Fd())
	session.Wait()
	return nil
}

// termSize gets the current window size and returns it in a window-change friendly
// format.
func termSize(fd uintptr) []byte {
	size := make([]byte, 16)

	winsize, err := term.GetWinsize(fd)
	if err != nil {
		binary.BigEndian.PutUint32(size, uint32(80))
		binary.BigEndian.PutUint32(size[4:], uint32(24))
		return size
	}

	binary.BigEndian.PutUint32(size, uint32(winsize.Width))
	binary.BigEndian.PutUint32(size[4:], uint32(winsize.Height))

	return size
}
