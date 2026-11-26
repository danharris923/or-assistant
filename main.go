// OR Assistant: a small local AI assistant that talks to OpenRouter directly.
// One binary serves a local web page and runs the model's tool calls as the
// current (non-root) user. There is no third-party backend in between.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `or-assistant %s - local AI assistant using OpenRouter

Usage:
  or-assistant start [--allow-root]               start the background server (or reuse it) and print its URL
  or-assistant serve [--port N] [--allow-root]    run the server in the foreground
  or-assistant stop                               stop the background server
  or-assistant version

The API key is read from OPENROUTER_API_KEY or set on the page (stored in
~/.config/or-assistant/config.json, mode 0600).
`, version)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "start":
		err = cmdStart(args)
	case "serve":
		err = cmdServe(args)
	case "stop":
		err = cmdStop()
	case "version", "--version", "-V":
		fmt.Println("or-assistant", version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "or-assistant:", err)
		os.Exit(1)
	}
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "/"
}

func configDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(base, "or-assistant")
}

func checkRoot(allow bool) error {
	if isRoot() && !allow {
		return errors.New("refusing to run as root: the AI could then change anything on this system. " +
			"Run it as a normal user (the launcher uses 'guest'), or pass --allow-root if you really mean it")
	}
	return nil
}

type runInfo struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func runFile() string { return filepath.Join(configDir(), "run.json") }

func readRun() (*runInfo, error) {
	b, err := os.ReadFile(runFile())
	if err != nil {
		return nil, err
	}
	var r runInfo
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *runInfo) url() string { return fmt.Sprintf("http://127.0.0.1:%d/?k=%s", r.Port, r.Token) }

func (r *runInfo) call(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", r.Port, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", r.Token)
	return (&http.Client{Timeout: 3 * time.Second}).Do(req)
}

func (r *runInfo) alive() bool {
	resp, err := r.call("GET", "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cmdStart reuses a running server or starts a detached one, then prints its URL.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	allowRoot := fs.Bool("allow-root", false, "allow running as root")
	fs.Parse(args)
	if err := checkRoot(*allowRoot); err != nil {
		return err
	}
	if r, err := readRun(); err == nil && r.alive() {
		fmt.Println(r.url())
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(configDir(), "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cargs := []string{"serve", "--print-url"}
	if *allowRoot {
		cargs = append(cargs, "--allow-root")
	}
	c := exec.Command(self, cargs...)
	c.Stderr = logf
	detach(c)
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if !strings.HasPrefix(s, "http://") {
			return fmt.Errorf("server failed to start; see %s", filepath.Join(configDir(), "server.log"))
		}
		fmt.Print(s)
	case <-time.After(15 * time.Second):
		return errors.New("timed out waiting for the server to start")
	}
	return c.Process.Release()
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "port on 127.0.0.1 (0 = random)")
	allowRoot := fs.Bool("allow-root", false, "allow running as root")
	printURL := fs.Bool("print-url", false, "print the URL then detach stdout (used by start)")
	fs.Parse(args)
	if err := checkRoot(*allowRoot); err != nil {
		return err
	}
	app, err := newApp()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	ri := runInfo{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok)}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(ri)
	if err := writeFileAtomic(runFile(), b, 0o600); err != nil {
		return err
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	srv.Handler = app.routes(ri.Port, ri.Token, func() {
		go func() {
			time.Sleep(200 * time.Millisecond)
			srv.Close()
		}()
	})
	fmt.Println(ri.url())
	if *printURL {
		os.Stdout.Close()
		os.Stdout, _ = os.Open(os.DevNull)
	}
	log.Printf("or-assistant %s listening on 127.0.0.1:%d as %s", version, ri.Port, currentUser())
	err = srv.Serve(ln)
	if cur, rerr := readRun(); rerr == nil && cur.Token == ri.Token {
		os.Remove(runFile())
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func cmdStop() error {
	r, err := readRun()
	if err != nil {
		fmt.Println("not running")
		return nil
	}
	resp, err := r.call("POST", "/internal/shutdown")
	if err != nil {
		os.Remove(runFile())
		fmt.Println("not running")
		return nil
	}
	resp.Body.Close()
	fmt.Println("stopped")
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
