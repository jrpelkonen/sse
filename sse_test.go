package sse

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanLines(t *testing.T) {
	inputs := []string{"event:foo", "data:bar", ":", "", "id:1234"}
	input := fmt.Sprintf("%s\r%s\r\n%s\n%s\n%s", inputs[0], inputs[1], inputs[2], inputs[3], inputs[4])
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(scanLines)

	i := 0
	for scanner.Scan() {
		result := scanner.Text()
		if inputs[i] != result {
			t.Fatalf(`scanLines = %q, want match for %#q, nil`, result, inputs[i])
		}
		i += 1
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Invalid input: %s", err)
	}
}

func TestScanEvent(t *testing.T) {
	inputs := []string{"event:foo", "data:bar", ":", "", "", "id:1234", "retry:3412"}
	expectedEvents := []SSE{{event: "foo", data: "bar"}, {id: "1234", retry: 3412}}
	input := strings.Join(inputs, "\r\n")
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(scanLines)

	for _, expected := range expectedEvents {
		event := scanEvent(scanner)
		if !reflect.DeepEqual(expected, *event) {
			t.Fatalf(`scanEvent = %q, expected %q`, event, expected)
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Invalid input: %s", err)
	}
}

func TestSSEToString(t *testing.T) {
	expectedStrings := []string{"event:foo\r\ndata:bar\r\n\r\n", "\r\n", "id:1234\r\nretry:3412\r\n\r\n"}
	events := []SSE{{event: "foo", data: "bar"}, {}, {id: "1234", retry: 3412}}
	for i, expectedString := range expectedStrings {
		actualString := events[i].String()
		if expectedString != actualString {
			t.Fatalf(`scanEvent = %q, expected %q`, actualString, expectedString)
		}
	}
}

type SlowReader struct {
	// no atomic boolean, use int32 as a thread safe flag
	closed int32
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (sr *SlowReader) Read(p []byte) (n int, err error) {
	for atomic.LoadInt32(&sr.closed) == 0 {
		sr.mu.Lock()
		n, err := sr.buffer.Read(p)
		sr.mu.Unlock()
		if err != io.EOF {
			return n, err
		} else {
			time.Sleep(time.Microsecond)
		}
	}
	return 0, io.EOF
}

func (sr *SlowReader) WriteString(s string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.buffer.WriteString(s)
}

func (sr *SlowReader) Close() {
	atomic.StoreInt32(&sr.closed, 1)
}

func TestToEventChan(t *testing.T) {
	inputs := []string{"event:foo", "data:bar", ":", "", "", "id:1234", "retry:3412"}
	expectedEvents := []SSE{{event: "foo", data: "bar"}, {id: "1234", retry: 3412}}
	var buffer SlowReader
	go func() {
		for _, line := range inputs {
			buffer.WriteString(line)
			time.Sleep(time.Millisecond)
			buffer.WriteString("\r\n")
			time.Sleep(time.Millisecond)
		}
		buffer.Close()
	}()

	eventChannel := toEventChan(&buffer)
	for _, expected := range expectedEvents {
		event := <-eventChannel
		if !reflect.DeepEqual(expected, event) {
			t.Fatalf(`toEventChan = %q, expected %q`, event, expected)
		}
	}
}

func TestHandler(t *testing.T) {
	handler := NewHandler()

	setup := func() (context.CancelFunc, *httptest.ResponseRecorder) {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "GET", "/test", nil)
		if err != nil {
			t.Fatal(err)
		}

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return cancel, rr
	}

	cancel1, rr1 := setup()
	cancel2, rr2 := setup()

	handler.AddDataEvent("test")
	cancel1()
	c1 := Client(rr1.Result())

	handler.AddDataEvent("test2")

	cancel2()

	c2 := Client(rr2.Result())

	messages1 := make([]SSE, 0)
	for sse := range c1 {
		messages1 = append(messages1, sse)
	}

	messages2 := make([]SSE, 0)
	for sse := range c2 {
		messages2 = append(messages2, sse)
	}

	if len(messages1) != 1 {
		t.Fatalf(`length = %q, expected %q`, len(messages1), 1)
	}

	if messages1[0].data != "test" {
		t.Fatalf(`length = %q, expected %q`, len(messages1), 1)
	}
	if len(messages2) != 2 {
		t.Fatalf(`length = %q, expected %q`, len(messages2), 2)
	}

}
