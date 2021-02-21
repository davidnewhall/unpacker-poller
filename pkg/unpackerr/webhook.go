package unpackerr

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"golift.io/cnfg"
	"golift.io/version"
)

// WebhookConfig defines the data to send webhooks to a server.
type WebhookConfig struct {
	Name       string          `json:"name" toml:"name" xml:"name" yaml:"name"`
	URL        string          `json:"url" toml:"url" xml:"url" yaml:"url"`
	CType      string          `json:"content_type" toml:"content_type" xml:"content_type" yaml:"content_type"`
	TmplPath   string          `json:"template_path" toml:"template_path" xml:"template_path" yaml:"template_path"`
	Timeout    cnfg.Duration   `json:"timeout" toml:"timeout" xml:"timeout" yaml:"timeout"`
	IgnoreSSL  bool            `json:"ignore_ssl" toml:"ignore_ssl" xml:"ignore_ssl" yaml:"ignore_ssl"`
	Silent     bool            `json:"silent" toml:"silent" xml:"silent" yaml:"silent"`
	Events     []ExtractStatus `json:"events" toml:"events" xml:"events" yaml:"events"`
	Exclude    []string        `json:"exclude" toml:"exclude" xml:"exclude" yaml:"exclude"`
	nickname   string
	client     *http.Client
	fails      uint
	posts      uint
	sync.Mutex `json:"-" toml:"-" xml:"-" yaml:"-"`
}

// Errors produced by this file.
var (
	ErrInvalidStatus = fmt.Errorf("invalid HTTP status reply")
)

func (u *Unpackerr) sendWebhooks(i *Extract) {
	if i.Status == IMPORTED && i.App == FolderString {
		return // This is an internal state change we don't need to fire on.
	}

	payload := &WebhookPayload{
		Path:  i.Path,
		App:   i.App,
		IDs:   i.IDs,
		Time:  i.Updated,
		Data:  nil,
		Event: i.Status,
		// Application Metadata.
		Go:       runtime.Version(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Version:  version.Version,
		Revision: version.Revision,
		Branch:   version.Branch,
		Started:  version.Started,
	}

	if i.Status <= EXTRACTED && i.Resp != nil {
		payload.Data = &XtractPayload{
			Archives: append(i.Resp.Extras, i.Resp.Archives...),
			Files:    i.Resp.NewFiles,
			Start:    i.Resp.Started,
			Output:   i.Resp.Output,
			Bytes:    i.Resp.Size,
			Elapsed:  cnfg.Duration{Duration: i.Resp.Elapsed},
		}

		if i.Resp.Error != nil {
			payload.Data.Error = i.Resp.Error.Error()
		}
	}

	for _, hook := range u.Webhook {
		if !hook.HasEvent(i.Status) || hook.Excluded(i.App) {
			continue
		}

		go u.sendWebhookWithLog(hook, payload)
	}
}

func (u *Unpackerr) sendWebhookWithLog(hook *WebhookConfig, payload *WebhookPayload) {
	tmpl, err := hook.Template()
	if err != nil {
		u.Printf("[ERROR] Webhook Template (%s = %s): %v", payload.Path, payload.Event, err)
	}

	var body bytes.Buffer
	if err = tmpl.Execute(&body, payload); err != nil {
		u.Printf("[ERROR] Webhook Payload (%s = %s): %v", payload.Path, payload.Event, err)
	} else if reply, err := hook.Send(&body); err != nil {
		u.Printf("[ERROR] Webhook (%s = %s): %v", payload.Path, payload.Event, err)
	} else if !hook.Silent {
		u.Printf("[Webhook] Posted Payload (%s = %s): %s: OK", payload.Path, payload.Event, hook.Name)
		u.Debugf("[DEBUG] Webhook Response: %s", string(bytes.ReplaceAll(reply, []byte{'\n'}, []byte{' '})))
	}
}

// Send marshals an interface{} into json and POSTs it to a URL.
func (w *WebhookConfig) Send(body io.Reader) ([]byte, error) {
	w.Lock()
	defer w.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), w.Timeout.Duration+time.Second)
	defer cancel()

	b, err := w.send(ctx, body)
	if err != nil {
		w.fails++
	}

	w.posts++

	return b, err
}

func (w *WebhookConfig) send(ctx context.Context, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request '%s': %w", w.Name, err)
	}

	req.Header.Set("content-type", w.CType)

	res, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POSTing payload '%s': %w", w.Name, err)
	}
	defer res.Body.Close()

	// The error is mostly ignored because we don't care about the body.
	// Read it in to avoid a memopry leak. Used in the if-stanza below.
	reply, _ := ioutil.ReadAll(res.Body)

	if res.StatusCode < http.StatusOK || res.StatusCode > http.StatusNoContent {
		return nil, fmt.Errorf("%w (%s) '%s': %s", ErrInvalidStatus, res.Status, w.Name, reply)
	}

	return reply, nil
}

func (u *Unpackerr) validateWebhook() {
	for i := range u.Webhook {
		if u.Webhook[i].nickname = u.Webhook[i].Name; u.Webhook[i].Name == "" {
			u.Webhook[i].nickname = fmt.Sprintf("WebhookURL%d", i)
			u.Webhook[i].Name = u.Webhook[i].URL
		}

		if len(u.Webhook[i].nickname) > 80 { //nolint:gomnd // max discord nick length
			u.Webhook[i].nickname = u.Webhook[i].nickname[:80]
		}

		if u.Webhook[i].CType == "" {
			u.Webhook[i].CType = "application/json"
		}

		if u.Webhook[i].Timeout.Duration == 0 {
			u.Webhook[i].Timeout.Duration = u.Timeout.Duration
		}

		if len(u.Webhook[i].Events) == 0 {
			u.Webhook[i].Events = []ExtractStatus{WAITING}
		}

		if u.Webhook[i].client == nil {
			u.Webhook[i].client = &http.Client{
				Timeout: u.Webhook[i].Timeout.Duration,
				Transport: &http.Transport{TLSClientConfig: &tls.Config{
					InsecureSkipVerify: u.Webhook[i].IgnoreSSL, // nolint: gosec
				}},
			}
		}
	}
}

func (u *Unpackerr) logWebhook() {
	var ex string

	if c := len(u.Webhook); c == 1 {
		if u.Webhook[0].TmplPath != "" {
			ex = fmt.Sprintf(", template: %s, content_type: %s", u.Webhook[0].TmplPath, u.Webhook[0].CType)
		}

		u.Printf(" => Webhook Config: 1 URL: %s, timeout: %v, ignore ssl: %v, silent: %v%s, events: %v",
			u.Webhook[0].Name, u.Webhook[0].Timeout, u.Webhook[0].IgnoreSSL, u.Webhook[0].Silent, ex,
			logEvents(u.Webhook[0].Events))
	} else {
		u.Print(" => Webhook Configs:", c, "URLs")

		for _, f := range u.Webhook {
			if ex = ""; f.TmplPath != "" {
				ex = fmt.Sprintf(", template: %s, content_type: %s", f.TmplPath, f.CType)
			}

			u.Printf(" =>    URL: %s, timeout: %v, ignore ssl: %v, silent: %v%s, events: %v",
				f.Name, f.Timeout, f.IgnoreSSL, f.Silent, ex, logEvents(f.Events))
		}
	}
}

// logEvents is only used in logWebhook to format events for printing.
func logEvents(events []ExtractStatus) (s string) {
	if len(events) == 1 && events[0] == WAITING {
		return "all"
	}

	for _, e := range events {
		if len(s) > 0 {
			s += "; "
		}

		s += e.String()
	}

	return s
}

// Excluded returns true if an app is in the Exclude slice.
func (w *WebhookConfig) Excluded(app string) bool {
	for _, a := range w.Exclude {
		if strings.EqualFold(a, app) {
			return true
		}
	}

	return false
}

// HasEvent returns true if a status event is in the Events slice.
// Also returns true if the Events slice has only one value of WAITING.
func (w *WebhookConfig) HasEvent(e ExtractStatus) bool {
	for _, h := range w.Events {
		if (h == WAITING && len(w.Events) == 1) || h == e {
			return true
		}
	}

	return false
}

// WebhookCounts returns the total count of requests and errors for all webhooks.
func (u *Unpackerr) WebhookCounts() (total uint, fails uint) {
	for _, hook := range u.Webhook {
		t, f := hook.Counts()
		total += t
		fails += f
	}

	return total, fails
}

// Counts returns the total count of requests and failures for a webhook.
func (w *WebhookConfig) Counts() (uint, uint) {
	w.Lock()
	defer w.Unlock()

	return w.posts, w.fails
}
