package ignatius

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultWorkersAIBase is Cloudflare's REST API root.
const DefaultWorkersAIBase = "https://api.cloudflare.com/client/v4"

// WorkersAI is a Backend for a System One model hosted on Cloudflare Workers AI (Clef:
// "@cf/cloudflare/clef" and "@cf/cloudflare/clef-flash"). It is not the /v1/systemone wire: the
// model is in the URL, the answers come wrapped in Cloudflare's {"result", "success", "errors"}
// envelope, and the credential is an account API token.
//
//	POST {BaseURL}/accounts/{AccountID}/ai/run/{Model}     Authorization: Bearer {APIKey}
//
// Checked against Cloudflare: Clef's input and output schemas (`cf ai get-model-schema`), its
// requirement that images be data URLs (a raw base64 string is refused: "image must be an embedded
// base64 data URI"), and a real call through a Worker. NOT yet checked: the REST envelope around the
// output, so decoding is tolerant (answers under "result" or at the top level, and a missing answer
// type is filled from the question). TestRealClefWorkersAI checks it when you have an API token.
type WorkersAI struct {
	BaseURL   string // default DefaultWorkersAIBase
	AccountID string
	Model     string // e.g. "@cf/cloudflare/clef-flash"
	APIKey    string
	Client    *http.Client
	Timeout   time.Duration
}

func (w *WorkersAI) Call(ctx context.Context, req Request) (WireResponse, error) {
	if w.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.Timeout)
		defer cancel()
	}
	// `model` is a required field of Clef's input, besides being in the URL (its schema is
	// `cf ai get-model-schema --model @cf/cloudflare/clef-flash`).
	payload := map[string]any{"model": w.Model[strings.LastIndex(w.Model, "/")+1:], "state": req.State, "questions": req.Questions}
	if len(req.Images) > 0 {
		uris := make([]string, len(req.Images))
		for i, img := range req.Images {
			u, ok := workersImage(img)
			if !ok {
				return WireResponse{}, &CallError{Kind: KindUnsupported, Message: fmt.Sprintf("images[%d] is not a PNG, JPEG or WebP image", i)}
			}
			uris[i] = u
		}
		payload["images"] = uris
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return WireResponse{}, &CallError{Kind: KindDecode, Message: err.Error()}
	}
	base := w.BaseURL
	if base == "" {
		base = DefaultWorkersAIBase
	}
	endpoint := strings.TrimRight(base, "/") + "/accounts/" + w.AccountID + "/ai/run/" + w.Model
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return WireResponse{}, &CallError{Kind: KindTransport, Message: "invalid Workers AI URL"} // the URL holds the account id
	}
	hreq.Header.Set("Content-Type", "application/json")
	if w.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+w.APIKey)
	}
	c := w.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(hreq)
	if err != nil {
		var ue *url.Error // its text carries the URL, which holds the account id: keep only the cause
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return WireResponse{}, err // Registry.call classifies timeouts vs transport
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return WireResponse{}, err
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return WireResponse{}, &CallError{Kind: KindHTTP, Message: fmt.Sprintf("%d: %s", resp.StatusCode, snippet), Status: resp.StatusCode}
	}
	return decodeWorkersAI(raw, req)
}

// decodeWorkersAI reads Cloudflare's envelope. The answers may be under "result" (the documented
// envelope) or at the top level (a response that is already the System One shape).
func decodeWorkersAI(raw []byte, req Request) (WireResponse, error) {
	var env struct {
		Result  json.RawMessage `json:"result"`
		Success *bool           `json:"success"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
		Answers map[string]Answer `json:"answers"`
		Usage   Usage             `json:"usage"`
		Model   string            `json:"model"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return WireResponse{}, &CallError{Kind: KindDecode, Message: err.Error()}
	}
	if env.Success != nil && !*env.Success { // a 200 that says it failed; the codes are Cloudflare's numeric ones
		codes := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			codes = append(codes, fmt.Sprint(e.Code))
		}
		return WireResponse{}, &CallError{Kind: KindHTTP, Message: "workers-ai reported failure (codes " + strings.Join(codes, ",") + ")"}
	}
	wire := WireResponse{Model: env.Model, Answers: env.Answers, Usage: env.Usage}
	if len(env.Result) > 0 && string(env.Result) != "null" {
		var inner struct {
			Answers map[string]Answer `json:"answers"`
			Usage   Usage             `json:"usage"`
			Model   string            `json:"model"`
		}
		if err := json.Unmarshal(env.Result, &inner); err != nil {
			return WireResponse{}, &CallError{Kind: KindDecode, Message: err.Error()}
		}
		wire.Answers, wire.Usage, wire.Model = inner.Answers, inner.Usage, inner.Model
	}
	if len(wire.Answers) == 0 {
		return WireResponse{}, &CallError{Kind: KindDecode, Message: "no answers in the Workers AI response"}
	}
	for id, a := range wire.Answers {
		if a.Type == "" { // take the type from the question when the response leaves it out
			a.Type = req.Questions[id].Type
			wire.Answers[id] = a
		}
	}
	return wire, nil
}

// workersImage turns a raw base64 image (what the Jev wire and Ollama take) into the data URL Workers AI
// requires, labeled by what the bytes are. A data URL passes through. ok is false for anything that is
// not a PNG, JPEG or WebP, which is all Clef reads.
func workersImage(img string) (string, bool) {
	if len(img) >= 5 && strings.EqualFold(img[:5], "data:") {
		return img, true
	}
	img = strings.Join(strings.Fields(img), "")
	head := img
	if len(head) > 24 {
		head = head[:24] // 18 bytes: enough to tell, and a multiple of 4 characters
	}
	b, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return "", false
	}
	var mime string
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		mime = "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		mime = "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		mime = "image/webp"
	default:
		return "", false
	}
	return "data:" + mime + ";base64," + img, true
}
