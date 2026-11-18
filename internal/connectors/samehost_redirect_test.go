package connectors

import (
	"net/http"
	"testing"
)

func TestSameHostRedirectPolicy(t *testing.T) {
	policy := SameHostRedirect("https://api.github.com")
	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("request %s: %v", raw, err)
		}
		return req
	}
	via := []*http.Request{request("https://api.github.com/repos/o/r")}

	if err := policy(request("https://api.github.com/repositories/1"), via); err != nil {
		t.Fatalf("same-host redirect = %v, want allowed", err)
	}
	if err := policy(request("https://evil.example/repositories/1"), via); err != http.ErrUseLastResponse {
		t.Fatalf("cross-host redirect = %v, want ErrUseLastResponse", err)
	}
	if err := policy(request("http://api.github.com/repositories/1"), via); err != http.ErrUseLastResponse {
		t.Fatalf("scheme change = %v, want ErrUseLastResponse", err)
	}
	loop := []*http.Request{request("https://api.github.com/a"), request("https://api.github.com/b"), request("https://api.github.com/c")}
	if err := policy(request("https://api.github.com/d"), loop); err == nil {
		t.Fatal("fourth redirect was allowed")
	}
}
