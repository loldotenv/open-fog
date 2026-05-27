// Package fr24 talks to flightradar24.com via TLS impersonation, sidestepping
// Cloudflare's JA3/JA4 fingerprint check. Plain net/http (Node fetch, curl,
// stock Python requests) returns 403 "Just a moment..." — bogdanfinn/utls
// forges Chrome's ClientHello so we get through.
package fr24

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

type Client struct {
	http tls_client.HttpClient
}

func New() (*Client, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_146),
		// We only call known API/HTML endpoints — a redirect from FR24 means
		// either a Cloudflare challenge interstitial or an upstream surprise.
		// Both are worth surfacing rather than silently following.
		tls_client.WithNotFollowRedirects(),
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("init tls-client: %w", err)
	}
	return &Client{http: c}, nil
}

// PlaybackJSON keeps the wire shape we need from the FR24 response.
// The actual JSON is much richer; we only parse what the KML conversion needs.
type PlaybackJSON struct {
	Result struct {
		Response struct {
			Data struct {
				Flight Flight `json:"flight"`
			} `json:"data"`
		} `json:"response"`
	} `json:"result"`
}

type Flight struct {
	Identification struct {
		Callsign *string `json:"callsign"`
		Number   struct {
			Default string `json:"default"`
		} `json:"number"`
	} `json:"identification"`
	Airport struct {
		Origin      Airport `json:"origin"`
		Destination Airport `json:"destination"`
	} `json:"airport"`
	Track []TrackPoint `json:"track"`
}

type Airport struct {
	Name string `json:"name"`
	Code struct {
		IATA string `json:"iata"`
		ICAO string `json:"icao"`
	} `json:"code"`
	Position struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Region    struct {
			City string `json:"city"`
		} `json:"region"`
	} `json:"position"`
}

type TrackPoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  struct {
		Meters int `json:"meters"`
	} `json:"altitude"`
	Timestamp int64 `json:"timestamp"`
}

// FetchPlayback pulls the playback JSON for a (flightId, timestamp) pair.
// Both are required — FR24's API returns an empty track if you only pass the
// hex. The timestamp comes from the row's `data-timestamp` in the history HTML.
func (c *Client) FetchPlayback(flightId string, timestamp int64) (*PlaybackJSON, error) {
	if !validHex(flightId) {
		return nil, fmt.Errorf("invalid flightId %q", flightId)
	}
	u := fmt.Sprintf("https://api.flightradar24.com/common/v1/flight-playback.json?flightId=%s&timestamp=%d",
		strings.ToLower(flightId), timestamp)
	body, err := c.get(u, "https://www.flightradar24.com/")
	if err != nil {
		return nil, err
	}
	var pb PlaybackJSON
	if err := json.Unmarshal(body, &pb); err != nil {
		return nil, fmt.Errorf("decode playback: %w (first 200 bytes: %q)", err, string(truncate(body, 200)))
	}
	if len(pb.Result.Response.Data.Flight.Track) == 0 {
		return nil, errors.New("playback returned but track is empty (flight may not have flown yet, or the (flightId, timestamp) pair is wrong)")
	}
	return &pb, nil
}

// Candidate is one row of the FR24 flight-history table. Multiple candidates
// can match a (number, date) query — tag flights with the same number flying
// two legs the same day, day-of disruption renumbering, etc.
type Candidate struct {
	FlightID  string `json:"flightId"`  // hex
	Timestamp int64  `json:"timestamp"` // unix, departure (STD)
	FromCity  string `json:"fromCity,omitempty"`
	FromIATA  string `json:"fromIATA,omitempty"`
	ToCity    string `json:"toCity,omitempty"`
	ToIATA    string `json:"toIATA,omitempty"`
	HasATD    bool   `json:"hasATD"` // false = scheduled-only, no playback yet
	OffsetSec int    `json:"offsetSec"` // local TZ offset for STD display
}

// Pre-compiled patterns for parsing the rows.
var (
	rowRe       = regexp.MustCompile(`(?s)<tr[^>]*data-timestamp="(\d+)"[^>]*>(.*?)</tr>`)
	hashRe      = regexp.MustCompile(`(?i)href="[^"]*#([0-9a-f]{6,10})"`)
	fromRe      = regexp.MustCompile(`<label>FROM</label>\s*<span[^>]*>\s*([^<]+?)\s*<a[^>]*>\s*\(([A-Z0-9]{3,4})\)`)
	toRe        = regexp.MustCompile(`<label>TO</label>\s*<span[^>]*>\s*([^<]+?)\s*<a[^>]*>\s*\(([A-Z0-9]{3,4})\)`)
	atdRe       = regexp.MustCompile(`<label>ATD</label>\s*<span[^>]*data-timestamp="(\d+)"`)
	stdOff      = regexp.MustCompile(`<label>STD</label>\s*<span[^>]*data-offset="(-?\d+)"`)
	validHexRe  = regexp.MustCompile(`^[0-9a-f]{6,10}$`)
)

// FetchCandidates scrapes the flight-history page for `number` and returns
// every leg in the table. Caller filters by date.
func (c *Client) FetchCandidates(number string) ([]Candidate, error) {
	u := fmt.Sprintf("https://www.flightradar24.com/data/flights/%s", strings.ToLower(number))
	body, err := c.get(u, "https://www.flightradar24.com/")
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, m := range rowRe.FindAllSubmatch(body, -1) {
		rowBody := m[2]
		hm := hashRe.FindSubmatch(rowBody)
		if hm == nil {
			continue
		}
		ts, perr := strconv.ParseInt(string(m[1]), 10, 64)
		if perr != nil {
			continue
		}
		cand := Candidate{
			FlightID:  strings.ToLower(string(hm[1])),
			Timestamp: ts,
		}
		if f := fromRe.FindSubmatch(rowBody); f != nil {
			cand.FromCity = strings.TrimSpace(string(f[1]))
			cand.FromIATA = string(f[2])
		}
		if t := toRe.FindSubmatch(rowBody); t != nil {
			cand.ToCity = strings.TrimSpace(string(t[1]))
			cand.ToIATA = string(t[2])
		}
		if a := atdRe.FindSubmatch(rowBody); a != nil && len(a[1]) > 0 {
			cand.HasATD = true
		}
		if o := stdOff.FindSubmatch(rowBody); o != nil {
			if off, err := strconv.Atoi(string(o[1])); err == nil {
				cand.OffsetSec = off
			}
		}
		out = append(out, cand)
	}
	return out, nil
}

// LookupCandidatesByDate returns every leg whose departure UTC date matches.
func (c *Client) LookupCandidatesByDate(number, date string) ([]Candidate, error) {
	target, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fmt.Errorf("bad date %q (want YYYY-MM-DD)", date)
	}
	all, err := c.FetchCandidates(number)
	if err != nil {
		return nil, err
	}
	// Zero rows from the scrape almost always means the flight number is
	// unknown OR FR24's HTML changed and rowRe no longer matches — surface
	// that separately so the user isn't sent chasing dates.
	if len(all) == 0 {
		return nil, fmt.Errorf("FR24 returned no flight rows for %q — flight number may be wrong, or the history page format may have changed", number)
	}
	var matches []Candidate
	for _, r := range all {
		// Compare against the row's *local* date at the origin airport. FR24
		// displays the history table in local time, so a user typing "2026-05-21"
		// expects flights whose origin-local departure date is 2026-05-21,
		// not whose UTC departure date is. Shift the unix epoch by the row's
		// offset, then read the calendar date in UTC.
		shifted := time.Unix(r.Timestamp+int64(r.OffsetSec), 0).UTC()
		if shifted.Year() == target.Year() && shifted.YearDay() == target.YearDay() {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no leg for %s on %s (FR24 returned %d other legs for this number; free tier shows ~7 days)", number, date, len(all))
	}
	return matches, nil
}


func validHex(s string) bool {
	return validHexRe.MatchString(strings.ToLower(s))
}

// get does a Chrome-impersonated GET and returns the body bytes.
// Returns a wrapped error for 4xx/5xx, including a Cloudflare hint on 403/503.
func (c *Client) get(url, referer string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Header order matters for fingerprinting — match Chrome's.
	// Note: the user-agent's Chrome version is intentionally kept in sync with
	// the profile (Chrome_146) since Cloudflare cross-checks UA vs JA3.
	req.Header = http.Header{
		"sec-ch-ua":          {`"Google Chrome";v="146", "Not?A_Brand";v="8", "Chromium";v="146"`},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"macOS"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":         {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"},
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"sec-fetch-site":     {"none"},
		"sec-fetch-mode":     {"navigate"},
		"sec-fetch-user":     {"?1"},
		"sec-fetch-dest":     {"document"},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {"en-US,en;q=0.9"},
		"referer":            {referer},
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"upgrade-insecure-requests", "user-agent", "accept",
			"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
			"accept-encoding", "accept-language", "referer",
		},
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	// Bound the body — Cloudflare challenge pages can be hundreds of KB and
	// we only need the first 200 bytes for error messages.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		// 3xx means a redirect we declined to follow (see fr24.New) — usually
		// a Cloudflare challenge interstitial. 4xx/5xx are normal errors.
		hint := ""
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc := resp.Header.Get("Location")
			hint = fmt.Sprintf(" (redirect to %q — likely a Cloudflare challenge)", loc)
		case resp.StatusCode == 403 || resp.StatusCode == 503:
			hint = " (Cloudflare may have flagged the request; try a newer Chrome profile in fr24.New)"
		}
		return nil, fmt.Errorf("HTTP %d from %s%s — first 200 bytes: %q",
			resp.StatusCode, url, hint, string(truncate(body, 200)))
	}
	return body, nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
