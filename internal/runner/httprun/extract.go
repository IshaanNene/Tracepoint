package httprun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/tidwall/gjson"

	"github.com/IshaanNene/Tracepoint/internal/config"
)

// maxCapture bounds how much of a body is kept for expectations and extraction when
// the step sets no max_body_bytes. The rest is still read, so transfer time is
// measured and the connection can be reused; it just is not kept.
const maxCapture = 1 << 20

// capture reads a body to completion, keeping up to a bound of it.
func capture(body io.Reader, expect *config.Expect) ([]byte, int64, error) {
	keep := int64(maxCapture)
	if expect != nil && expect.MaxBodyBytes > 0 {
		keep = expect.MaxBodyBytes
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(body, keep))
	if err != nil {
		return buf.Bytes(), n, err
	}
	rest, err := io.Copy(io.Discard, body)
	if err != nil && !errors.Is(err, io.EOF) {
		return buf.Bytes(), n + rest, err
	}
	return buf.Bytes(), n + rest, nil
}

// jsonHolds checks every JSON assertion: a path that must or must not exist, or a
// value it must equal. A body that is not JSON satisfies nothing but "must not exist".
func jsonHolds(body []byte, checks []config.JSONExpect) bool {
	valid := gjson.ValidBytes(body)
	for _, c := range checks {
		res := gjson.Result{}
		if valid {
			res = gjson.GetBytes(body, c.Path)
		}
		if c.Exists != nil && res.Exists() != *c.Exists {
			return false
		}
		if c.Equals != nil && (!res.Exists() || !equal(res, c.Equals)) {
			return false
		}
	}
	return true
}

// equal compares a JSON value with the configured one by kind: numbers as numbers,
// so 1 and 1.0 agree; booleans as booleans; everything else as text.
func equal(res gjson.Result, want any) bool {
	switch w := want.(type) {
	case bool:
		return (res.Type == gjson.True || res.Type == gjson.False) && res.Bool() == w
	case int:
		return res.Type == gjson.Number && res.Float() == float64(w)
	case int64:
		return res.Type == gjson.Number && res.Float() == float64(w)
	case uint64:
		return res.Type == gjson.Number && res.Float() == float64(w)
	case float64:
		return res.Type == gjson.Number && res.Float() == w
	case string:
		return res.String() == w
	case nil:
		return res.Type == gjson.Null
	default:
		return res.Raw == fmt.Sprint(w) || res.String() == fmt.Sprint(w)
	}
}

// extractValue pulls one value out of a response. A value that is absent - a missing
// path, an absent header - is not found, and the step fails as extract_failed rather
// than handing an empty string to the next step.
func extractValue(e config.Extract, body []byte, resp *http.Response) (string, bool) {
	switch e.From {
	case "header":
		v := resp.Header.Get(e.Path)
		return v, v != ""
	case "status":
		return strconv.Itoa(resp.StatusCode), true
	default:
		if !gjson.ValidBytes(body) {
			return "", false
		}
		res := gjson.GetBytes(body, e.Path)
		if !res.Exists() || res.Type == gjson.Null {
			return "", false
		}
		return res.String(), true
	}
}
