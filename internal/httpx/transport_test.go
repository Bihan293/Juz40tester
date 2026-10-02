package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestNewTransportTuned(t *testing.T) {
	tr := NewTransport()
	if tr.MaxIdleConns != MaxIdleConns || tr.MaxIdleConnsPerHost != MaxIdleConnsPerHost || tr.IdleConnTimeout != IdleConnTimeout {
		t.Fatalf("pool settings not applied: %+v", tr)
	}
	if tr.Proxy == nil {
		t.Fatal("proxy from environment must be preserved")
	}
	if tr == http.DefaultTransport {
		t.Fatal("must not mutate http.DefaultTransport")
	}
	if d := http.DefaultTransport.(*http.Transport); d.MaxIdleConnsPerHost == MaxIdleConnsPerHost {
		t.Fatal("http.DefaultTransport was modified")
	}
	c := NewClient(15 * time.Second)
	if c.Timeout != 15*time.Second || c.Transport == nil {
		t.Fatalf("client: %+v", c)
	}
}
