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
	event string
	data  string
	id    string
	retry uint64
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
	condWrite(&sb, "event", sse.event)
	condWrite(&sb, "data", sse.data)
	condWrite(&sb, "id", sse.id)
	if sse.retry > 0 {
		condWrite(&sb, "retry", strconv.FormatUint(sse.retry, 10))
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
			keyValue := strings.Split(line, ":")
			if len(keyValue) == 2 {
				switch keyValue[0] {
				case "event":
					event.event = keyValue[1]
				case "data":
					event.data = keyValue[1]
				case "id":
					event.id = keyValue[1]
				case "retry":
					retry, err := strconv.ParseUint(keyValue[1], 10, 64)
					if err == nil {
						event.retry = retry
					}
				}
			}
		}
	}
	return event
}

func NewDataEvent(data string) SSE {
	return SSE{data: data}
}

// server

type InflightRequest struct {
	writer  http.ResponseWriter
	request *http.Request
}

type Handler struct {
	mu        sync.Mutex
	requests  []InflightRequest
	events    chan<- SSE
	event_ack <-chan bool
}

func (handler *Handler) withRequests(fn func(requests []InflightRequest) []InflightRequest) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.requests = fn(handler.requests)
}

var headers = map[string]string{
	"Cache-Control": "no-cache",
	"Content-Type":  "text/event-stream",
}

func (server *Handler) Handle(writer http.ResponseWriter, r *http.Request) {
	server.withRequests(func(requests []InflightRequest) []InflightRequest {
		return append(requests, InflightRequest{writer: writer, request: r})
	})

	go func() {
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

	}()
	for name, value := range headers {
		writer.Header().Add(name, value)
	}

	writer.Write([]byte{})
}

func (server *Handler) AddDataEvent(data string) {
	server.AddEvent(NewDataEvent(data))
}

func (server *Handler) AddEvent(event SSE) {
	server.events <- event
	<-server.event_ack
}

func NewHandler() *Handler {
	events := make(chan SSE)
	event_ack := make(chan bool)
	server := Handler{requests: make([]InflightRequest, 0), events: events, event_ack: event_ack}

	go func() {
		handleEvent := func(event SSE) {
			server.withRequests(func(requests []InflightRequest) []InflightRequest {
				var wg sync.WaitGroup
				defer wg.Wait()
				buf := event.String()

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
			event_ack <- true
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