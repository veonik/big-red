package main

import (
	"bytes"
	"code.google.com/p/go.crypto/ssh"
	"encoding/json"
	"github.com/go-martini/martini"
	"github.com/martini-contrib/render"
	"io"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"sync"
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

type LastRun struct {
	Error     string
	StartTime *time.Time
}

type AppState struct {
	Working       bool
	StartTime     *time.Time
	Configuration AppConfiguration
	Logger        *log.Logger
	LastRun       LastRun
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

	return &AppState{false, nil, config, logger, LastRun{"", nil}}
}

func main() {
	state := NewAppState()

	m := martini.Classic()
	m.Use(render.Renderer())

	m.Get("/", func(r render.Render) {
		r.HTML(200, "index", state)
	})

	m.Post("/press", func(r render.Render) {
		if !state.Working {
			go state.PerformDump()
		}

		r.JSON(200, nil)
	})

	m.Get("/status", func(r render.Render) {
		r.JSON(200, map[string]interface{}{"working": state.Working, "startedAt": state.StartTime, "lastRun": map[string]interface{}{"error": state.LastRun.Error, "startedAt": state.LastRun.StartTime}})
	})

	m.Run()
}

func (state *AppState) PerformDump() {
	defer func() {
		if r := recover(); r != nil {
			original, ok := r.(string)
			if ok {
				state.LastRun.Error = original
			} else {
				original, ok := r.(error)
				if ok {
					state.LastRun.Error = original.Error()
				}
			}

			buf := make([]byte, 1<<16)
			runtime.Stack(buf, false)
			state.Logger.Println(r)
			state.Logger.Println(buf)
		} else {
			state.LastRun.Error = ""
		}

		state.LastRun.StartTime = state.StartTime

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

	stdoutReader, err := sourceSession.StdoutPipe()
	if err != nil {
		panic("Could create pipe: " + err.Error())
	}

	stdinWriter, err := destSession.StdinPipe()
	if err != nil {
		panic("Could create pipe: " + err.Error())
	}

	loggerWriter := LoggerWriter{state.Logger}
	destSession.Stdout = loggerWriter
	destSession.Stderr = loggerWriter

	sourceSession.Stderr = loggerWriter

	if err := sourceSession.Start(state.Configuration.Source.Command); err != nil {
		panic("Failed to run source command: " + err.Error())
	}

	if err := destSession.Start(state.Configuration.Destination.Command); err != nil {
		panic("Failed to run destination command: " + err.Error())
	}

	storage := bytes.Buffer{}
	reading := true
	bytesRead := 0
	bytesWritten := 0
	mutex := &sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		state.Logger.Println("Starting read")

		buf := make([]byte, 4096)
		for {
			n, err := stdoutReader.Read(buf)
			if err != nil && err != io.EOF {
				panic(err)
			}
			if n == 0 {
				break
			}

			mutex.Lock()
			bytesRead += n
			mutex.Unlock()

			mutex.Lock()
			_, err = storage.Write(buf[0:n])
			mutex.Unlock()
			if err != nil {
				panic(err)
			}
		}

		mutex.Lock()
		reading = false
		mutex.Unlock()

		if err := sourceSession.Wait(); err != nil {
			panic("Source command failed: " + err.Error())
		}

		state.Logger.Println("Finished reading source. Total size:", bytesRead, "bytes")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		state.Logger.Println("Starting write")

		buf := make([]byte, 1<<21) // 2MiB
		for {
			mutex.Lock()
			currentlyReading := reading
			currentLength := storage.Len()
			mutex.Unlock()

			if currentlyReading && currentLength < cap(buf) {
				continue

			} else if !currentlyReading && currentLength <= 0 {
				break
			}

			mutex.Lock()
			n, err := storage.Read(buf)
			mutex.Unlock()
			if err != nil {
				panic(err)
			}
			
			nw, err := stdinWriter.Write(buf[0:n])
			if err != nil {
				panic(err)
			}

			bytesWritten += n
			state.Logger.Println("Read", n, "bytes", "Wrote", nw, "bytes")
		}

		state.Logger.Println("Finished writing destination. Total size:", bytesWritten, "bytes")
		state.Logger.Println("Waiting for destination command to complete")

		stdinWriter.Close()
		if err := destSession.Wait(); err != nil {
			panic("Destination command failed: " + err.Error())
		}
	}()

	wg.Wait()
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
