/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package jprqconfig resolves the jprq base domain used to build a tunnel's
// public URL.
//
// The agent learns its public hostname from the jprq server at connect time and
// prints it as unstructured text on stdout (cli/jprqc.go), and it deliberately
// has no API access to report it back (automountServiceAccountToken: false,
// DESIGN.md §6.3). So the controller composes the URL itself from the assigned
// subdomain plus the base domain fetched here — the same remote config the agent
// reads in cli/config.go.
package jprqconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultURL is the jprq remote config endpoint, matching cli/config.go.
const DefaultURL = "https://jprq.io/config.json"

const fetchTimeout = 10 * time.Second

// Resolver returns the jprq base domain, fetching it at most once per process.
//
// The domain is cosmetic: it only decorates status.url. A failed fetch must
// therefore never block pod creation — callers are expected to log the error,
// leave status.url empty and requeue.
type Resolver struct {
	// URL of the remote config. Defaults to DefaultURL when empty.
	URL string

	// Override short-circuits the fetch entirely. Set by --jprq-domain, which
	// keeps tests and envtest hermetic.
	Override string

	mu     sync.Mutex
	cached string
}

// NewResolver builds a Resolver. An empty url falls back to DefaultURL; a
// non-empty override means the network is never touched.
func NewResolver(url, override string) *Resolver {
	if url == "" {
		url = DefaultURL
	}
	return &Resolver{URL: url, Override: override}
}

// Domain returns the jprq base domain, e.g. "jprq.live".
//
// Only successful fetches are cached, so a transient failure is retried on the
// next reconcile rather than poisoning the process.
func (r *Resolver) Domain(ctx context.Context) (string, error) {
	if r.Override != "" {
		return r.Override, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != "" {
		return r.cached, nil
	}

	url := r.URL
	if url == "" {
		url = DefaultURL
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: unexpected status %s", url, resp.Status)
	}

	var body struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding %s: %w", url, err)
	}
	if body.Domain == "" {
		return "", fmt.Errorf("%s returned an empty domain", url)
	}

	r.cached = body.Domain
	return r.cached, nil
}

// URLFor composes the public URL for a subdomain, e.g.
// https://muzaffar-web.jprq.live.
func URLFor(subdomain, domain string) string {
	return fmt.Sprintf("https://%s.%s", subdomain, domain)
}
