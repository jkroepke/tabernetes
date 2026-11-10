package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"

	utilexec "k8s.io/utils/exec"
)

// The image huggingface.co/mlc-ai/<model> runs a language model with WebLLM
// (https://webllm.mlc.ai) on the GPU of the browser. The model is a WebLLM
// model id, and that repository on Hugging Face has its weights: the tab
// pulls them from there and keeps them in the Cache Storage of the browser. The pod
// serves the OpenAI chat API on port 8000, and "chat" answers on kubectl exec.

const (
	webllmPort    = 8000
	webllmRepo    = "huggingface.co/mlc-ai/"
	defaultModel  = "Qwen2.5-1.5B-Instruct-q4f16_1-MLC"
	webllmPrompts = "You are a helpful assistant. You run in a browser tab, on the GPU through WebGPU, " +
		"as a pod on a Kubernetes node that is the browser tab itself. Answer briefly."
)

type webLLM struct {
	tp    *tabPod
	model string
	h     js.Value   // the handle of gpu.js start()
	gen   sync.Mutex // one answer at a time
	ln    net.Listener
	funcs []js.Func
}

func newWebLLM(tp *tabPod, model string) (workload, error) {
	if model == "" || model == "latest" {
		model = defaultModel
	}
	return &webLLM{tp: tp, model: model}, nil
}

// webllmModel returns the model of a WebLLM image:
// huggingface.co/mlc-ai/<model>, or webllm:<model> as before.
func webllmModel(image string) (string, bool) {
	if model, ok := strings.CutPrefix(image, webllmRepo); ok && model != "" {
		return model, true
	}
	if name, tag, _ := strings.Cut(image, ":"); name == "webllm" || name == "tabernetes.dev/webllm" {
		return tag, true
	}
	return "", false
}

func (m *webLLM) imageID() string { return webllmRepo + m.model }

func (m *webLLM) runtime() string { return "webgpu" }

// fetchedMB matches the progress of WebLLM while it downloads the weights.
var fetchedMB = regexp.MustCompile(`^Fetching param cache\[\d+/\d+\]: (\d+)MB fetched`)

func (m *webLLM) start(ctx context.Context, log io.Writer, pull *imagePull) error {
	gpu := js.Global().Get("tabernetesGPU")
	if gpu.Type() != js.TypeObject {
		return errors.New("this page has no GPU support")
	}
	// The weights are the image: downloading them is the pull, and the
	// container starts with the shaders. The log begins there.
	var size int64
	progress := js.FuncOf(func(_ js.Value, a []js.Value) any {
		text := a[0].String()
		switch {
		case strings.HasPrefix(text, "Fetching param cache"):
			pull.fetching()
			if mb := fetchedMB.FindStringSubmatch(text); mb != nil {
				n, _ := strconv.ParseInt(mb[1], 10, 64)
				size = n << 20
			}
			return nil
		case strings.HasPrefix(text, "Loading model from cache"):
			return nil
		}
		if !pull.wasPulled() {
			pull.done(size)
			fmt.Fprintf(log, "loading %s with WebLLM on %s\n", m.model, m.tp.n.gpu)
		}
		fmt.Fprintln(log, text)
		return nil
	})
	m.funcs = append(m.funcs, progress)
	m.h = gpu.Call("start", m.model, progress)
	if _, err := awaitPromise(ctx, m.h.Get("ready")); err != nil {
		return err
	}
	pull.done(size)
	ln, err := net.Listen("tcp", net.JoinHostPort(m.tp.ip, strconv.Itoa(webllmPort)))
	if err != nil {
		return err
	}
	m.ln = ln
	go (&http.Server{Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	fmt.Fprintf(log, "ready, the OpenAI chat API listens on %s\n", ln.Addr())
	return nil
}

func (m *webLLM) stop() {
	if m.ln != nil {
		m.ln.Close()
	}
	if m.h.Type() == js.TypeObject {
		m.h.Call("stop")
	}
	// Release the funcs a little later: the worker may still have sent
	// progress that has not been delivered yet.
	funcs := m.funcs
	m.funcs = nil
	time.AfterFunc(time.Minute, func() {
		for _, f := range funcs {
			f.Release()
		}
	})
}

func (m *webLLM) dial(port int32) (net.Conn, error) {
	if port != webllmPort {
		return nil, fmt.Errorf("nothing listens on port %d, webllm serves %d", port, webllmPort)
	}
	return net.Dial("tcp", net.JoinHostPort(m.tp.ip, strconv.Itoa(webllmPort)))
}

// complete sends an OpenAI chat completion request to WebLLM. With
// "stream": true, onChunk gets each chunk as JSON and the result is empty;
// otherwise the result is the response as JSON.
func (m *webLLM) complete(ctx context.Context, req map[string]any, onChunk func(string)) (string, error) {
	m.gen.Lock()
	defer m.gen.Unlock()
	delete(req, "model") // the worker has one
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	// The chunks arrive in a JS callback, which must not block: a goroutine
	// hands them on.
	var (
		mu     sync.Mutex
		queue  []string
		signal = make(chan struct{}, 1)
	)
	cb := js.FuncOf(func(_ js.Value, a []js.Value) any {
		mu.Lock()
		queue = append(queue, a[0].String())
		mu.Unlock()
		select {
		case signal <- struct{}{}:
		default:
		}
		return nil
	})
	finished := make(chan struct{})
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		for {
			select {
			case <-signal:
			case <-finished:
			}
			mu.Lock()
			q := queue
			queue = nil
			mu.Unlock()
			for _, c := range q {
				if onChunk != nil {
					onChunk(c)
				}
			}
			select {
			case <-finished:
				mu.Lock()
				left := len(queue)
				mu.Unlock()
				if left == 0 {
					return
				}
			default:
			}
		}
	}()
	p := m.h.Call("chat", string(b), cb)
	v, err := awaitPromise(ctx, p)
	if ctx.Err() != nil {
		// Stop the answer, and wait for the worker to say so before the
		// callback goes away.
		m.h.Call("interrupt")
		wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		awaitPromise(wait, p)
		cancel()
	}
	close(finished)
	<-delivered
	cb.Release()
	if err != nil {
		return "", err
	}
	if v.Type() != js.TypeString {
		return "", nil
	}
	return v.String(), nil
}

// chatDelta is the part of a streamed chunk the chat command shows.
type chatDelta struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (m *webLLM) exec(ctx context.Context, cmd []string, in io.Reader, out, errOut io.Writer) error {
	if len(cmd) == 0 || cmd[0] != "chat" {
		name := ""
		if len(cmd) > 0 {
			name = cmd[0]
		}
		fmt.Fprintf(errOut, "exec: %q: not found. The webllm image has one command:\n  chat [question]\n", name)
		return utilexec.CodeExitError{Err: fmt.Errorf("%q not found", name), Code: 127}
	}
	history := []chatMessage{{Role: "system", Content: webllmPrompts}}
	answer := func(ctx context.Context, q string) error {
		history = append(history, chatMessage{Role: "user", Content: q})
		var text strings.Builder
		_, err := m.complete(ctx, map[string]any{"messages": history, "stream": true}, func(chunk string) {
			var d chatDelta
			// after Ctrl-C, the rest of the answer is not wanted
			if ctx.Err() == nil && json.Unmarshal([]byte(chunk), &d) == nil && len(d.Choices) > 0 {
				text.WriteString(d.Choices[0].Delta.Content)
				io.WriteString(out, d.Choices[0].Delta.Content)
			}
		})
		io.WriteString(out, "\n")
		history = append(history, chatMessage{Role: "assistant", Content: text.String()})
		return err
	}
	if len(cmd) > 1 {
		if err := answer(ctx, strings.Join(cmd[1:], " ")); err != nil {
			fmt.Fprintln(errOut, "chat:", err)
			return utilexec.CodeExitError{Err: err, Code: 1}
		}
		return nil
	}

	// One question per line, until stdin ends. Without a tty, chat is its
	// own line discipline: Ctrl-C (\x03) on stdin stops the answer and chat.
	lines := make(chan string, 64)
	interrupt := make(chan struct{})
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if strings.ContainsRune(sc.Text(), '\x03') {
				close(interrupt)
				return
			}
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		io.WriteString(out, "> ")
		var q string
		select {
		case l, ok := <-lines:
			if !ok {
				io.WriteString(out, "\n")
				return nil
			}
			q = strings.TrimSpace(l)
		case <-interrupt:
			return nil
		case <-ctx.Done():
			return nil
		}
		if q == "" {
			continue
		}
		actx, stop := context.WithCancel(ctx)
		go func() {
			select {
			case <-interrupt:
				stop()
			case <-actx.Done():
			}
		}()
		err := answer(actx, q)
		stop()
		select {
		case <-interrupt:
			return nil
		default:
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(errOut, "chat:", err)
		}
	}
}

//go:embed chat.html
var chatHTML string

// chatPage is the chat of GET / in a browser, on top of the API.
var chatPage = template.Must(template.New("chat").Parse(chatHTML))

// handler serves the OpenAI chat API, as far as WebLLM has it.
func (m *webLLM) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// A browser gets a chat page, curl and wget the API.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			chatPage.Execute(w, struct{ Model string }{m.model})
			return
		}
		fmt.Fprintf(w, "%s on WebGPU (WebLLM). OpenAI API:\n  GET /v1/models\n  POST /v1/chat/completions\n", m.model)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": m.model, "object": "model", "owned_by": "webllm"}},
		})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			_, err := m.complete(r.Context(), req, func(chunk string) {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				if flusher != nil {
					flusher.Flush()
				}
			})
			if err != nil {
				b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error()}})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		resp, err := m.complete(r.Context(), req, nil)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	})
	return mux
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
}
