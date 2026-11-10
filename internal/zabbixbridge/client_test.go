package zabbixbridge

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"testing"
	"time"
)

func tlsPool(t *testing.T, server *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return pool
}

func TestZabbixHistoryPushRejectsPartialOrMalformedResults(t *testing.T) {
	responses := []string{
		`{"jsonrpc":"2.0","result":{"response":"success","data":[{"itemid":"10600"},{"error":"Item is disabled."}]},"id":1}`,
		`{"jsonrpc":"2.0","result":{"response":"success","data":[{"itemid":"10600"},{"itemid":"10601"}]},"extra":1,"id":1}`,
		`{"jsonrpc":"2.0","result":{"response":"success","data":[{"itemid":"10600"},{"itemid":"10601"}]},"id":1}`,
	}
	i := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer z-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers=%v", r.Header)
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(responses[i]))
		i++
	}))
	defer s.Close()
	z := zabbixClient{url: s.URL, token: "z-token", client: newHTTPClient(tlsPool(t, s), time.Second), maxBody: 1024}
	values := []historyValue{{Host: "h", Key: "k", Value: "v", Clock: 1, NS: 2}, {Host: "h", Key: "k2", Value: "v2", Clock: 1, NS: 3}}
	if err := z.push(context.Background(), values); err == nil {
		t.Fatal("accepted partial result")
	}
	if err := z.push(context.Background(), values); err == nil {
		t.Fatal("accepted unknown response field")
	}
	if err := z.push(context.Background(), values); err != nil {
		t.Fatal(err)
	}
}
