// open-fog HTTP server.
//
// Endpoints:
//   GET  /                                              embedded index.html
//   GET  /api/candidates?flight=<num>&date=<YYYY-MM-DD> list matching legs
//   GET  /api/track?flightId=<hex>&timestamp=<unix>     fetch + convert one
//   GET  /api/rail/stations?name=<name>                 OSM station candidates
//   GET  /api/rail?fromLat=&fromLon=&toLat=&toLon=&fromName=&toName=
//                  [&date=<YYYY-MM-DD>][&highspeed=false] OSM-routed rail track
//                  (date defaults to today UTC; appears in filename + docName.
//                   highspeed defaults to true — prefer Shinkansen/high-speed
//                   lines; pass false to route the conventional network)
//
// The two-step shape lets the frontend disambiguate when a flight number flies
// multiple legs the same day (tag flights, disruption renumbering).
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/loldotenv/open-fog/internal/fr24"
	"github.com/loldotenv/open-fog/internal/kml"
	"github.com/loldotenv/open-fog/internal/rail"
)

// normalizeFlight strips ALL whitespace (not just leading/trailing) and
// uppercases the result. Users often type "QF 12" with an internal space, or
// paste from a boarding pass with formatting. FR24 expects a contiguous token.
func normalizeFlight(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s))
}

//go:embed static
var staticFS embed.FS

func main() {
	// Vercel's Go runtime injects PORT and expects the server to listen on it.
	// Locally PORT is unset, so we keep the :8080 default; -addr still overrides.
	defaultAddr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultAddr = ":" + port
	}
	addr := flag.String("addr", defaultAddr, "listen address")
	flag.Parse()

	client, err := fr24.New()
	if err != nil {
		log.Fatalf("init fr24 client: %v", err)
	}
	railClient := rail.New()

	siteFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(siteFS))
	mux.HandleFunc("GET /api/candidates", candidatesHandler(client))
	mux.HandleFunc("GET /api/track", trackHandler(client))
	mux.HandleFunc("GET /api/rail/stations", railStationsHandler(railClient))
	mux.HandleFunc("GET /api/rail", railHandler(railClient))

	log.Printf("open-fog listening on http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// candidatesHandler returns the list of legs matching a (number, date) query.
// One row = one Candidate. Frontend decides 1 → auto-fetch vs >1 → picker.
func candidatesHandler(client *fr24.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		num := normalizeFlight(r.URL.Query().Get("flight"))
		date := r.URL.Query().Get("date")
		if num == "" || date == "" {
			writeError(w, http.StatusBadRequest, "need ?flight=<num>&date=<YYYY-MM-DD>")
			return
		}
		cs, err := client.LookupCandidatesByDate(num, date)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"candidates": cs})
	}
}

// trackHandler fetches a specific leg's playback and converts it to KML.
// Both flightId and timestamp are required — FR24's API returns empty track
// without the paired timestamp.
func trackHandler(client *fr24.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flightId := r.URL.Query().Get("flightId")
		tsStr := r.URL.Query().Get("timestamp")
		if flightId == "" || tsStr == "" {
			writeError(w, http.StatusBadRequest, "need ?flightId=<hex>&timestamp=<unix>")
			return
		}
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "timestamp must be a unix integer")
			return
		}
		pb, err := client.FetchPlayback(flightId, ts)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		res, err := kml.Convert(pb)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// railStationsHandler resolves a station name into OSM candidates. The
// frontend renders these for the user to confirm before routing.
func railStationsHandler(client *rail.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			writeError(w, http.StatusBadRequest, "need ?name=<station>")
			return
		}
		stations, err := client.LookupStations(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"stations": stations})
	}
}

// railHandler routes between two pre-resolved stations and returns a
// Fog of World-compatible KML built from the rail right-of-way (no GPS log).
// Coordinates and names come from /api/rail/stations — the server doesn't
// re-geocode here, so the user's confirmed pick is what gets routed.
func railHandler(client *rail.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		src, srcErr := parseStation(q, "from")
		dst, dstErr := parseStation(q, "to")
		if srcErr != nil || dstErr != nil {
			writeError(w, http.StatusBadRequest, "need ?fromLat=&fromLon=&fromName=&toLat=&toLon=&toName=")
			return
		}
		// Date is optional — defaults to today UTC. Only ever shows up in the
		// filename + docName (Fog of World ignores the in-track timestamps).
		var start time.Time
		if date := q.Get("date"); date != "" {
			parsed, err := time.Parse("2006-01-02", date)
			if err != nil {
				writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
				return
			}
			start = parsed
		} else {
			start = time.Now().UTC()
		}
		// Default journey-start hour: 09:00 UTC. The exact time doesn't matter
		// for Fog of World (timestamps are ignored for tile uncovering) but
		// must be present and monotonically increasing in the gx:Track.
		start = time.Date(start.Year(), start.Month(), start.Day(), 9, 0, 0, 0, time.UTC)

		// Prefer high-speed lines (Shinkansen, TGV, …) by default so an inter-city
		// route follows the high-speed alignment instead of the parallel
		// conventional line. The frontend toggle sends highspeed=false to opt out.
		preferHS := q.Get("highspeed") != "false"

		res, err := client.GenerateFromCoords(r.Context(), src, dst, start, preferHS)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// parseStation reads a Station triple (lat, lon, name) from query params
// prefixed with `from` or `to`. Empty/invalid coords are an error — these
// must always come from a confirmed /api/rail/stations pick.
//
// OSMID/OSMType are optional pass-through so the response round-trips the
// fields the frontend pin popup wants to display. The router itself only
// needs coords; missing IDs are not an error.
func parseStation(q url.Values, prefix string) (rail.Station, error) {
	latStr := q.Get(prefix + "Lat")
	lonStr := q.Get(prefix + "Lon")
	name := strings.TrimSpace(q.Get(prefix + "Name"))
	if latStr == "" || lonStr == "" || name == "" {
		return rail.Station{}, fmt.Errorf("%s: missing lat/lon/name", prefix)
	}
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return rail.Station{}, fmt.Errorf("%s lat: %w", prefix, err)
	}
	// ParseFloat accepts "NaN", "+Inf", "-Inf" without erroring — those then
	// poison bbox math, haversine, and Dijkstra. Range-check explicitly
	// (range comparisons against NaN are always false, so check NaN first).
	if math.IsNaN(lat) || lat < -90 || lat > 90 {
		return rail.Station{}, fmt.Errorf("%s lat out of range: %q", prefix, latStr)
	}
	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		return rail.Station{}, fmt.Errorf("%s lon: %w", prefix, err)
	}
	if math.IsNaN(lon) || lon < -180 || lon > 180 {
		return rail.Station{}, fmt.Errorf("%s lon out of range: %q", prefix, lonStr)
	}
	s := rail.Station{Name: name, Latitude: lat, Longitude: lon}
	if id := q.Get(prefix + "OsmId"); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			s.OSMID = n
		}
	}
	if t := q.Get(prefix + "OsmType"); t == "N" || t == "W" || t == "R" {
		s.OSMType = t
	}
	if r := strings.TrimSpace(q.Get(prefix + "Region")); r != "" {
		s.Region = r
	}
	return s, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
