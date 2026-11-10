package zabbixbridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type metric struct {
	Identity    string            `json:"identity,omitempty"`
	Name        string            `json:"name"`
	Value       json.RawMessage   `json:"value"`
	ValueType   string            `json:"value_type"`
	Unit        string            `json:"unit,omitempty"`
	Description string            `json:"description,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
	Labels      map[string]string `json:"labels,omitempty"`
}
type definition struct {
	OID         string            `json:"oid"`
	Name        string            `json:"name,omitempty"`
	Type        string            `json:"type,omitempty"`
	Unit        string            `json:"unit,omitempty"`
	Description string            `json:"description,omitempty"`
	Enum        map[string]string `json:"enum,omitempty"`
}
type snapshot struct {
	Name             string       `json:"name"`
	Driver           string       `json:"driver"`
	Available        bool         `json:"available"`
	Stale            bool         `json:"stale"`
	LastAttempt      time.Time    `json:"last_attempt,omitempty"`
	LastSuccess      time.Time    `json:"last_success,omitempty"`
	UnavailableSince time.Time    `json:"unavailable_since"`
	PollDuration     int64        `json:"poll_duration_ns"`
	Error            string       `json:"error,omitempty"`
	Metrics          []metric     `json:"metrics"`
	Discovery        []definition `json:"discovery,omitempty"`
}
type sourceStatus struct {
	Name             string    `json:"name"`
	Driver           string    `json:"driver"`
	Available        bool      `json:"available"`
	Stale            bool      `json:"stale"`
	LastAttempt      time.Time `json:"last_attempt,omitempty"`
	LastSuccess      time.Time `json:"last_success,omitempty"`
	UnavailableSince time.Time `json:"unavailable_since"`
	PollDuration     int64     `json:"poll_duration_ns"`
	Error            string    `json:"error,omitempty"`
}
type metricRecord struct {
	Operation string  `json:"operation"`
	Identity  string  `json:"identity"`
	Heartbeat bool    `json:"heartbeat,omitempty"`
	Metric    *metric `json:"metric,omitempty"`
}
type transaction struct {
	Sequence         uint64         `json:"sequence"`
	Cursor           string         `json:"cursor"`
	CapturedAt       time.Time      `json:"captured_at"`
	Source           string         `json:"source"`
	Status           sourceStatus   `json:"status"`
	Metrics          []metricRecord `json:"metrics"`
	DiscoveryChanged bool           `json:"discovery_changed,omitempty"`
	Discovery        []definition   `json:"discovery,omitempty"`
}
type fullResponse struct {
	SchemaVersion int        `json:"schema_version"`
	Generation    string     `json:"generation"`
	Cursor        string     `json:"cursor"`
	CapturedAt    time.Time  `json:"captured_at"`
	Sources       []snapshot `json:"sources"`
}
type deltaResponse struct {
	SchemaVersion int           `json:"schema_version"`
	FromCursor    string        `json:"from_cursor"`
	NextCursor    string        `json:"next_cursor"`
	HeadCursor    string        `json:"head_cursor"`
	HasMore       bool          `json:"has_more"`
	Transactions  []transaction `json:"transactions"`
}

type historyValue struct {
	Host  string `json:"host"`
	Key   string `json:"key"`
	Value string `json:"value"`
	Clock int64  `json:"clock"`
	NS    int    `json:"ns"`
}
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  []historyValue `json:"params"`
	ID      int            `json:"id"`
}
type rpcResponse struct {
	JSONRPC string     `json:"jsonrpc"`
	Result  *rpcResult `json:"result,omitempty"`
	Error   *rpcError  `json:"error,omitempty"`
	ID      int        `json:"id"`
}
type rpcResult struct {
	Response string          `json:"response"`
	Data     []rpcItemResult `json:"data"`
}
type rpcItemResult struct {
	ItemID string `json:"itemid,omitempty"`
	Error  string `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func newHTTPClient(roots *x509.CertPool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, Proxy: http.ProxyFromEnvironment, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: timeout},
	}
}
func rootsFor(caFile string) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		f, e := os.Open(caFile)
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		closeErr := f.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(b) > 4<<20 {
			return nil, fmt.Errorf("CA file too large")
		}
		if !roots.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
	}
	return roots, nil
}

func strictDecode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}
func readBounded(resp *http.Response, max int64) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return body, nil
}

type gatewayClient struct {
	baseURL, token string
	client         *http.Client
	maxBody        int64
}

func (g gatewayClient) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gateway request: %w", err)
	}
	body, err := readBounded(resp, g.maxBody)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
	}
	if err := strictDecode(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode gateway response: %w", err)
	}
	return resp.StatusCode, nil
}
func (g gatewayClient) full(ctx context.Context) (fullResponse, error) {
	var out fullResponse
	_, err := g.get(ctx, "/api/v1/metrics/all", &out)
	if err == nil {
		err = validateFull(out)
	}
	return out, err
}
func (g gatewayClient) delta(ctx context.Context, cursor string) (deltaResponse, int, error) {
	var out deltaResponse
	path := "/api/v1/metrics/delta?since=" + url.QueryEscape(cursor)
	code, err := g.get(ctx, path, &out)
	if err == nil {
		err = validateDelta(out, cursor)
	}
	return out, code, err
}
func validateFull(v fullResponse) error {
	parts := strings.Split(v.Cursor, ".")
	if v.SchemaVersion != 1 || v.Generation == "" || !cursorPattern.MatchString(v.Cursor) || len(parts) != 3 || parts[1] != v.Generation || v.CapturedAt.IsZero() || v.Sources == nil {
		return fmt.Errorf("invalid full response schema")
	}
	return nil
}
func validateDelta(v deltaResponse, cursor string) error {
	if v.SchemaVersion != 1 || v.FromCursor != cursor || v.NextCursor == "" || v.HeadCursor == "" || v.Transactions == nil {
		return fmt.Errorf("invalid delta response schema")
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid delta cursor")
	}
	generation := parts[1]
	expected, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || expected == math.MaxUint64 {
		return fmt.Errorf("invalid delta cursor sequence")
	}
	if !cursorPattern.MatchString(v.NextCursor) || !cursorPattern.MatchString(v.HeadCursor) || !strings.HasPrefix(v.NextCursor, "v1."+generation+".") || !strings.HasPrefix(v.HeadCursor, "v1."+generation+".") {
		return fmt.Errorf("delta cursor generation mismatch")
	}
	expected++
	for _, t := range v.Transactions {
		wantCursor := "v1." + generation + "." + strconv.FormatUint(t.Sequence, 10)
		if t.Cursor != wantCursor || t.Source == "" || t.Status.Name != t.Source || t.CapturedAt.IsZero() || t.Metrics == nil || t.Sequence != expected {
			return fmt.Errorf("invalid transaction or sequence gap")
		}
		expected++
	}
	if len(v.Transactions) > 0 && v.NextCursor != v.Transactions[len(v.Transactions)-1].Cursor {
		return fmt.Errorf("next cursor mismatch")
	}
	return nil
}

type zabbixClient struct {
	url, token string
	client     *http.Client
	maxBody    int64
}

func (z zabbixClient) push(ctx context.Context, values []historyValue) error {
	if len(values) == 0 {
		return nil
	}
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "history.push", Params: values, ID: 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+z.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := z.client.Do(req)
	if err != nil {
		return fmt.Errorf("zabbix request: %w", err)
	}
	data, err := readBounded(resp, z.maxBody)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zabbix returned HTTP %d", resp.StatusCode)
	}
	var out rpcResponse
	if err := strictDecode(data, &out); err != nil {
		return fmt.Errorf("decode zabbix response: %w", err)
	}
	if out.JSONRPC != "2.0" || out.ID != 1 || out.Error != nil || out.Result == nil {
		return fmt.Errorf("zabbix history.push failed")
	}
	r := out.Result
	if r.Response != "success" || len(r.Data) != len(values) {
		return fmt.Errorf("zabbix history.push returned %d results for %d values", len(r.Data), len(values))
	}
	for index, result := range r.Data {
		if result.Error != "" || result.ItemID == "" {
			return fmt.Errorf("zabbix history.push item %d failed", index)
		}
	}
	return nil
}
