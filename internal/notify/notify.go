// Package notify sends the user's notifications to ntfy.
package notify

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
)

// Ntfy posts to a topic URL; an empty URL turns notifications off.
type Ntfy struct {
	URL       string
	UserAgent string
	HTTP      *http.Client
}

func (n *Ntfy) Enabled() bool { return n != nil && n.URL != "" }

func (n *Ntfy) Send(ctx context.Context, title, body string) error {
	if !n.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", mime.BEncoding.Encode("UTF-8", title))
	req.Header.Set("User-Agent", n.UserAgent)
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}
