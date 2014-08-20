package main

import (
	"code.google.com/p/go.crypto/ssh"
	"encoding/json"
	"github.com/go-martini/martini"
	"github.com/martini-contrib/render"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"time"
)

type LoggerWriter struct {
	Logger *log.Logger
}

func (self LoggerWriter) Write(p []byte) (n int, err error) {
	self.Logger.Print(string(p))

	return len(p), nil
}

type EndpointConfiguration struct {
	User    string
	Host    string
	Command string
}

type AppConfiguration struct {
	PrivateKeyFile string
	AuthMethods    []ssh.AuthMethod
	Source         EndpointConfiguration
	Destination    EndpointConfiguration
}

type AppState struct {
	Working       bool
	StartTime     *time.Time
	Configuration AppConfiguration
	Logger        *log.Logger
}

func NewAppState() *AppState {
	file, err := os.Open("config.json")
	if err != nil {
		panic("Could not open config.json: " + err.Error())
	}

	decoder := json.NewDecoder(file)

	config := AppConfiguration{}
	if err := decoder.Decode(&config); err != nil {
		panic("Could not decode config.json: " + err.Error())
	}

	privKeyText, err := ioutil.ReadFile(config.PrivateKeyFile)
	if err != nil {
		panic("Failed to read private key: " + err.Error())
	}

	privKey, err := ssh.ParseRawPrivateKey(privKeyText)
	if err != nil {
		panic("Failed to parse private key: " + err.Error())
	}

	signer, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		panic("Failed to create signer: " + err.Error())
	}

	config.AuthMethods = []ssh.AuthMethod{
		ssh.PublicKeys(signer),
	}

	logger := log.New(os.Stdout, "[big-red] ", 0)

	return &AppState{false, nil, config, logger}
}

func main() {
	state := NewAppState()

	m := martini.Classic()
	m.Use(render.Renderer())

	m.Get("/", func(r render.Render) {
		r.HTML(200, "index", state)
	})

	m.Get("/press", func(r render.Render) {
		if !state.Working {
			go state.PerformDump()
		}

		r.Redirect("/")
	})

	m.Run()
}

func (state *AppState) PerformDump() {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 1<<16)
			runtime.Stack(buf, false)
			state.Logger.Println(r)
			state.Logger.Println(buf)
		}

		state.Logger.Println("Done. Took " + state.Elapsed().String())

		state.Working = false
		state.StartTime = nil
	}()

	start := time.Now()
	state.Working = true
	state.StartTime = &start

	state.Logger.Println("Started performing work")

	sourceSession := state.newSourceSession()
	defer sourceSession.Close()

	destSession := state.newDestinationSession()
	defer destSession.Close()

	stdoutPipe, err := sourceSession.StdoutPipe()
	if err != nil {
		panic("Could create pipe: " + err.Error())
	}

	loggerWriter := LoggerWriter{state.Logger}

	destSession.Stdin = stdoutPipe
	destSession.Stdout = loggerWriter
	destSession.Stderr = loggerWriter

	sourceSession.Stderr = loggerWriter

	if err := destSession.Start(state.Configuration.Destination.Command); err != nil {
		panic("Failed to run destination command: " + err.Error())
	}

	if err := sourceSession.Start(state.Configuration.Source.Command); err != nil {
		panic("Failed to run source command: " + err.Error())
	}

	sourceSession.Wait()
	destSession.Wait()
}

func (state *AppState) Elapsed() time.Duration {
	return time.Since(*state.StartTime)
}

func (state *AppState) newSourceSession() *ssh.Session {
	return state.newSession(state.Configuration.Source.User, state.Configuration.Source.Host)
}

func (state *AppState) newDestinationSession() *ssh.Session {
	return state.newSession(state.Configuration.Destination.User, state.Configuration.Destination.Host)
}

func (state *AppState) newSession(user string, address string) *ssh.Session {
	client := state.newClient(user, address)

	session, err := client.NewSession()
	if err != nil {
		panic("Failed to create session: " + err.Error())
	}

	return session
}

func (state *AppState) newClient(user string, address string) *ssh.Client {
	config := state.newClientConfig(user)

	client, err := ssh.Dial("tcp", address+":22", config)
	if err != nil {
		panic("Failed to dial: " + err.Error())
	}

	return client
}

func (state *AppState) newClientConfig(user string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: state.Configuration.AuthMethods,
	}
}
