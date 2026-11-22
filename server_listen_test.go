//go:build unix

package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// Without systemd's variables, or with ones meant for another process,
// there's no socket to take: serve listens on -port itself.
func TestSystemdListenerNone(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	if ln, err := systemdListener(); ln != nil || err != nil {
		t.Fatalf("no variables: got %v, %v", ln, err)
	}
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()+1))
	t.Setenv("LISTEN_FDS", "1")
	if ln, err := systemdListener(); ln != nil || err != nil {
		t.Fatalf("another process's socket: got %v, %v", ln, err)
	}
	if os.Getenv("LISTEN_PID") != "" || os.Getenv("LISTEN_FDS") != "" {
		t.Error("the variables should be cleared")
	}
}

// A socket passed as systemd does (descriptor 3, LISTEN_PID set to the
// process's own pid by the time it runs) is the one the server accepts on.
func TestSystemdListenerPassed(t *testing.T) {
	if os.Getenv("CF_SYSTEMD_LISTENER_CHILD") == "1" {
		ln, err := systemdListener()
		if err != nil || ln == nil {
			os.Stdout.WriteString("no listener\n")
			os.Exit(1)
		}
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		c.Write([]byte("ok\n"))
		c.Close()
		os.Exit(0)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// exec keeps the shell's pid, so $$ is the test binary's pid, as
	// systemd's LISTEN_PID is the service's.
	cmd := exec.Command("/bin/sh", "-c", `LISTEN_PID=$$ LISTEN_FDS=1 exec "$0" -test.run='^TestSystemdListenerPassed$'`, os.Args[0])
	cmd.Env = append(os.Environ(), "CF_SYSTEMD_LISTENER_CHILD=1")
	cmd.ExtraFiles = []*os.File{f} // descriptor 3
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ln.Close() // the child holds its own copy, as systemd and the service do
	defer cmd.Wait()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ok\n" {
		t.Fatalf("child answered %q, %v", line, err)
	}
}
