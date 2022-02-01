package sse

import (
	"bufio"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// data structures

type SSE struct {
	Event string
	Data  string
	Id    string
	Retry uint64
}

func condWrite(sb *strings.Builder, name string, value string) {
	if len(value) > 0 {
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(value)
		sb.WriteString("\r\n")
	}
}

func (sse SSE) String() string {
	var sb strings.Builder
	condWrite(&sb, "event", sse.Event)
	condWrite(&sb, "data", sse.Data)
	condWrite(&sb, "id", sse.Id)
	if sse.Retry > 0 {
		condWrite(&sb, "retry", strconv.FormatUint(sse.Retry, 10))
	}
	sb.WriteString("\r\n")
	return sb.String()
}

func isLineEnd(r rune) bool {
	return r == '\r' || r == '\n'
}

func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0
	for width, i := 0, start; i < len(data); i += width {
		var r rune
		r, width = utf8.DecodeRune(data[i:])
		if isLineEnd(r) {
			// if line end is '\r', check for subsequent '\n' and consume it if present
			if len(data) > i && r == '\r' && data[i+1] == '\n' {
				width += 1
			}
			return i + width, data[start:i], nil
		}
	}
	if atEOF && len(data) > start {
		return len(data), data[start:], nil
	}
	return start, nil, nil
}

func cut(s string, a rune) (string, string) {
	index := strings.IndexRune(s, a)
	if index == -1 {
		return s, ""
	}
	return s[:index], s[index+1:]
}

func scanEvent(scanner *bufio.Scanner) *SSE {
	var event *SSE
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			if event != nil {
				return event
			}
		} else {
			if event == nil {
				event = &SSE{}
			}
			key, value := cut(line, ':')
			if value != "" {
				switch key {
				case "event":
					event.Event = value
				case "data":
					event.Data = value
				case "id":
					event.Id = value
				case "retry":
					retry, err := strconv.ParseUint(value, 10, 64)
					if err == nil {
						event.Retry = retry
					}
				}
			}
		}
	}
	return event
}

func NewDataEvent(data string) SSE {
	return SSE{Data: data}
}

// server

type InflightRequest struct {
	writer  http.ResponseWriter
	request *http.Request
}

type Handler struct {
	mu              sync.Mutex
	requests        []InflightRequest
	events          chan<- SSE
	mostRecentEvent *SSE
	eventAck        <-chan bool
}

func (handler *Handler) withRequests(fn func(requests []InflightRequest) []InflightRequest) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.requests = fn(handler.requests)
}

var headers = map[string]string{
	"Cache-Control":     "no-cache",
	"Content-Type":      "text/event-stream",
	"Connection":        "keep-alive",
	"Transfer-Encoding": "identity",
}

func (server *Handler) ServeHTTP(writer http.ResponseWriter, r *http.Request) {
	server.withRequests(func(requests []InflightRequest) []InflightRequest {
		for name, value := range headers {
			writer.Header().Add(name, value)
		}
		writer.WriteHeader(200)
		if server.mostRecentEvent != nil {
			writer.Write([]byte(server.mostRecentEvent.String()))
			if f, ok := writer.(http.Flusher); ok {
				f.Flush()
			}
		}
		return append(requests, InflightRequest{writer: writer, request: r})
	})
	<-r.Context().Done()
	server.withRequests(func(requests []InflightRequest) []InflightRequest {
		for i, req := range requests {
			if req.request == r {
				requests = append(requests[:i], requests[i+1:]...)
				break
			}
		}
		return requests
	})
}

func (server *Handler) AddDataEvent(data string) {
	server.AddEvent(NewDataEvent(data))
}

func (server *Handler) AddEvent(event SSE) {
	server.events <- event
	<-server.eventAck
}

func NewHandler() *Handler {
	events := make(chan SSE)
	eventAck := make(chan bool)
	server := Handler{requests: make([]InflightRequest, 0), events: events, eventAck: eventAck}

	go func() {
		handleEvent := func(event SSE) {
			server.withRequests(func(requests []InflightRequest) []InflightRequest {
				var wg sync.WaitGroup
				defer wg.Wait()
				buf := event.String()
				server.mostRecentEvent = &event
				for _, req := range requests {
					wg.Add(1)
					go func(wg *sync.WaitGroup, w io.Writer) {
						defer wg.Done()
						w.Write([]byte(buf))

						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
					}(&wg, req.writer)

				}
				return requests
			})
		}
		for {
			event := <-events

			handleEvent(event)
			eventAck <- true
		}
	}()
	return &server
}

// Client
func Client(response *http.Response) <-chan SSE {
	return toEventChan(response.Body)
}

func toEventChan(r io.Reader) <-chan SSE {
	events := make(chan SSE, 1)

	scanner := bufio.NewScanner(r)
	scanner.Split(scanLines)
	go func() {
		defer close(events)
		finished := false
		for !finished {
			scannedEvent := scanEvent(scanner)
			if scannedEvent == nil {
				finished = true
			} else {
				events <- *scannedEvent
			}
		}
	}()
	return events
}
