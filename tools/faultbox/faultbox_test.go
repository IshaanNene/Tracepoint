package faultbox_test

import (
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/IshaanNene/Tracepoint/tools/faultbox"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("os/signal.loop"),
		goleak.IgnoreAnyFunction("os/signal.signalWaitUntilIdle"),
		// testcontainers keeps a connection to its reaper for the life of the process,
		// by design: the reaper removes the containers if the process dies.
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	)
}

func TestDurationJSON(t *testing.T) {
	var f faultbox.Fault
	if err := json.Unmarshal([]byte(`{"kind":"delay","delay":"250ms","at":"20s"}`), &f); err != nil {
		t.Fatal(err)
	}
	if time.Duration(f.Delay) != 250*time.Millisecond || time.Duration(f.At) != 20*time.Second {
		t.Fatalf("decoded %+v", f)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":0,"kind":"delay","at":"20s","delay":"250ms"}` {
		t.Fatalf("encoded %s", out)
	}
	if err := json.Unmarshal([]byte(`{"delay":"soon"}`), &f); err == nil {
		t.Fatal("a malformed duration must be refused")
	}
}
