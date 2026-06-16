// Package rail builds a Fog of World-compatible KML for a rail journey by
// stitching OSM railway geometry between two stations. It queries all
// `railway=rail|narrow_gauge` ways inside a bbox around the two stations
// and routes the shortest path over that subgraph.
//
// All data comes from the Overpass API (OpenStreetMap mirror). No train
// operator API, no GPS log — just the physical track right-of-way as mapped
// in OSM.
package rail

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// overpassMirrors are tried in order. Public Overpass instances regularly
// time out or 429 under load — falling through to the next one is more
// resilient than retrying the same endpoint.
var overpassMirrors = []string{
	"https://overpass-api.de/api/interpreter",
	"https://overpass.kumi.systems/api/interpreter",
	"https://overpass.private.coffee/api/interpreter",
}

// Overpass fetch budget. Public mirrors are slow and frequently hang, and the
// whole rail request runs inside a serverless function capped at 300 s (the
// Vercel Hobby ceiling, fluid compute enabled). Everything — all mirror
// attempts plus graph building — must finish under that, or the platform hard-
// kills the function with an opaque 504 before the mirror fallback or our own
// error messages ever run.
//
// Mirrors are queried with hedged requests rather than strict sequential
// fallback: we start the first mirror and, if it hasn't answered within
// overpassHedge, fire the next one concurrently and take whichever responds
// first (a failed attempt brings in the next immediately, without waiting out
// the hedge). A single congested or hung mirror no longer blocks the request
// for a full per-try timeout before the next is tried, so the documented
// 30–90 s tail — and the 504 it caused — collapses to roughly one hedge plus a
// healthy mirror's response time.
const (
	// overpassPerTry caps a single mirror attempt. Legitimate rail-network
	// queries take ~10–90 s depending on bbox size and mirror load; 75 s covers
	// the slow case and abandons a hung mirror within budget.
	overpassPerTry = 75 * time.Second

	// overpassHedge is how long an in-flight mirror has to answer before we also
	// launch the next one. Healthy mirrors return a typical inter-city query in
	// well under this (a large bbox measured ~10 s on a warm mirror), so the
	// common case still issues a single request; a mirror slower than this gets
	// raced instead of waited out.
	overpassHedge = 15 * time.Second

	// overpassBudget caps total wall-clock across all (now overlapping) mirror
	// attempts. Leaves ~70 s of the 300 s function ceiling for JSON decode,
	// graph build, Dijkstra, densify, and the response.
	overpassBudget = 230 * time.Second
)

// Tunables. Constants rather than knobs because the web form deliberately
// keeps things minimal — if these prove wrong we can iterate.
const (
	// bboxPadDeg pads the station bounding box on every side. 0.5° ≈ 55 km —
	// enough room for HSR lines that detour around mountains, while keeping
	// Overpass response sizes sane for typical inter-city journeys.
	bboxPadDeg = 0.5

	// bridgeToleranceM connects nearby disconnected graph nodes. Different
	// OSM ways at a junction sometimes don't share an exact coordinate;
	// 500 m bridges that gap without merging unrelated lines.
	bridgeToleranceM = 500.0

	// coordPrecision is the lat/lon rounding (in decimal places) used to
	// deduplicate way endpoints into shared graph nodes. 6 dp ≈ 11 cm.
	coordPrecision = 6

	// densifyTargetM is the max allowed gap between consecutive track
	// points. OSM often draws tunnels and viaducts as a single straight way
	// between two distant endpoints (gaps up to ~10 km observed on HSR
	// lines), which leaves Fog of World tiles (~1.1 km each) uncovered.
	// 200 m guarantees at least one point inside every tile along the route.
	// It is also the resample spacing for smooth() — note that smoothing fills
	// such a tunnel chord with points but cannot bend it back onto the real
	// curve, because OSM has no intermediate geometry there to curve toward.
	densifyTargetM = 200.0

	// catmullAlpha is the Catmull-Rom parameterisation used by smooth(). 0.5 is
	// the centripetal case — the only value mathematically proven free of cusps
	// and self-intersections, so corner-rounding never loops back on the track.
	catmullAlpha = 0.5
)

// Turn-cost tunables. A train cannot pivot at a node the way a shortest-path
// search over an undirected graph otherwise would: a switch diverges gently and
// a crossing is traversed straight through, but a ~90° turn onto a crossing line
// or a ~180° reversal at a junction is physically impossible. Routing over
// directed edges (state = "arrived at a node via a specific edge") lets us
// measure the heading change at each node and forbid turns sharper than
// maxTurnDeg — which removes the line-hops, crossing spikes, and bridge-edge
// jogs that otherwise render as straight "jumps" on the map. This is the same
// technique OSM rail routers (Geofabrik OpenRailRouting, the OSRM train profile)
// use; we port it into our Dijkstra rather than adopt a heavyweight engine.
const (
	// maxTurnDeg forbids any transition whose heading change exceeds it. 60°
	// clears real mapped curves (a few degrees per node) and gentle switch
	// divergence, while rejecting ~90° crossings and ~180° reversals.
	maxTurnDeg = 60.0

	// turnPenaltyM is a small per-degree tie-break added to edge cost so that,
	// where two continuations are both legal, the straighter one wins — which
	// suppresses residual hopping at shallow junctions.
	turnPenaltyM = 2.0
)

// High-speed-line preference. When the user opts in (the default), routing
// favours highspeed=yes ways (Shinkansen, TGV, ICE …) so an inter-city journey
// follows the high-speed alignment instead of the geometrically shorter
// conventional line that runs parallel to it. Two levers combine:
//
//   - highspeedDiscount scales the routing *cost* (not the reported distance) of
//     a high-speed edge, so the search prefers it unless the conventional
//     alternative is much shorter.
//   - the start/end are snapped to the nearest high-speed node (when one is in
//     range) rather than the nearest node overall — without this the cost
//     discount is unreachable, because boarding a grade-separated high-speed
//     line from a conventional platform requires a near-perpendicular connector
//     that the turn-angle cap rejects.
//
// A station with no high-speed line nearby falls back to ordinary routing, so
// short/rural journeys are unaffected.
const highspeedDiscount = 0.5

// maxSnapM caps how far a station may sit from the rail node it snaps to. Past
// this the bbox almost certainly doesn't contain a line that actually serves
// the station, so erroring beats routing from an unrelated track.
const maxSnapM = 5000.0

// Station is a geocoded OSM railway station.
type Station struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	OSMID     int64   `json:"osmId"`
	// OSMType is "N" (node), "W" (way), or "R" (relation). Used by the
	// frontend to build the correct openstreetmap.org/<type>/<id> link.
	OSMType string `json:"osmType,omitempty"`
	// Region is a human-readable disambiguation string ("Xi'an, Shaanxi,
	// China"). Empty when Photon has no admin context for the result.
	Region string `json:"region,omitempty"`
}

// Point is a single coordinate in the rendered track.
type Point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timestamp int64   `json:"timestamp"`
}

// Result is what the HTTP layer returns. Shape mirrors kml.Result (flight)
// so the frontend can share a renderer; Altitude is omitted because rail
// tracks are clampToGround.
type Result struct {
	KML         string  `json:"kml"`
	Filename    string  `json:"filename"`
	DocName     string  `json:"docName"`
	Callsign    string  `json:"callsign"`
	Origin      Station `json:"origin"`
	Destination Station `json:"destination"`
	Track       []Point `json:"track"`
	DistanceKM  float64 `json:"distanceKm"`
}

// Client is an Overpass-API client. Safe for concurrent use.
type Client struct {
	HTTP    *http.Client
	Mirrors []string

	// perTry / budget / hedge bound the Overpass fetch (defaults: overpassPerTry
	// / overpassBudget / overpassHedge). Fields rather than direct const use so
	// tests can drive the hedge and budget logic without real-time waits; zero
	// means default.
	perTry time.Duration
	budget time.Duration
	hedge  time.Duration
}

func New() *Client {
	return &Client{
		// Per-request context deadlines govern each call (overpassPerTry for
		// routing, photonTimeout for station lookup); this client-level timeout
		// is only an absolute backstop, kept under the 300 s function ceiling.
		HTTP:    &http.Client{Timeout: overpassBudget},
		Mirrors: overpassMirrors,
		// perTry/budget/hedge left zero — query() falls back to the consts. Tests
		// set them to drive the hedge and budget logic without real-time waits.
	}
}

// overpassResp is the minimal Overpass JSON shape we consume.
type overpassResp struct {
	Elements []overpassElem `json:"elements"`
}

type overpassElem struct {
	Type     string            `json:"type"`
	ID       int64             `json:"id"`
	Lat      float64           `json:"lat,omitempty"`
	Lon      float64           `json:"lon,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	Geometry []overpassGeomPt  `json:"geometry,omitempty"`
}

type overpassGeomPt struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func (c *Client) query(ctx context.Context, q string) (*overpassResp, error) {
	perTry, budget, hedge := c.perTry, c.budget, c.hedge
	if perTry == 0 {
		perTry = overpassPerTry
	}
	if budget == 0 {
		budget = overpassBudget
	}
	if hedge == 0 {
		hedge = overpassHedge
	}
	if len(c.Mirrors) == 0 {
		return nil, errors.New("overpass: no mirrors configured")
	}

	// One budget-bounded context shared by every hedged attempt. Returning a
	// winner fires the deferred cancel, which tears down the losing attempts so
	// their goroutines stop hammering the public mirrors and exit.
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type result struct {
		resp *overpassResp
		err  error
	}
	// Buffered to len(Mirrors): a losing goroutine must always be able to
	// deliver its result and exit, even after a winner has already returned.
	results := make(chan result, len(c.Mirrors))

	next, inFlight := 0, 0
	launchNext := func() {
		if next >= len(c.Mirrors) {
			return
		}
		endpoint := c.Mirrors[next]
		next++
		inFlight++
		go func() {
			// Per-mirror deadline within the shared budget: a hung mirror is
			// abandoned at perTry while the others keep racing.
			tctx, tcancel := context.WithTimeout(ctx, perTry)
			defer tcancel()
			v, err := c.queryOne(tctx, endpoint, q)
			results <- result{v, err}
		}()
	}

	hedgeTimer := time.NewTimer(hedge)
	defer hedgeTimer.Stop()
	resetHedge := func() {
		if !hedgeTimer.Stop() {
			select {
			case <-hedgeTimer.C:
			default:
			}
		}
		hedgeTimer.Reset(hedge)
	}

	launchNext()
	var lastErr error
	for inFlight > 0 {
		select {
		case <-ctx.Done():
			// Budget spent or caller cancelled (e.g. the function deadline
			// fired) — a clean ctx error beats running into the platform's
			// hard 300 s kill.
			return nil, ctx.Err()
		case <-hedgeTimer.C:
			// The leading mirror is slow; race the next one rather than wait it
			// out. The slow attempt keeps running — it may still win.
			launchNext()
			resetHedge()
		case res := <-results:
			inFlight--
			if res.err == nil {
				return res.resp, nil
			}
			// A mirror failed (per-try timeout, 429, decode error). Don't wait
			// for the hedge — bring in the next mirror immediately.
			lastErr = res.err
			launchNext()
			resetHedge()
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("overpass: no mirror succeeded within budget")
	}
	return nil, lastErr
}

func (c *Client) queryOne(ctx context.Context, endpoint, q string) (*overpassResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(url.Values{"data": {q}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	// Overpass mirrors rate-limit / 429 the default Go user-agent.
	req.Header.Set("user-agent", "open-fog/0.1 (rail-route; github.com/loldotenv/open-fog)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("overpass %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("overpass %s %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v overpassResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("overpass %s decode: %w", endpoint, err)
	}
	// Drain any trailing whitespace so the deferred Close lands on EOF and
	// the transport can reuse the keep-alive connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	return &v, nil
}

// maxStationCandidates caps the picker length. 10 is enough to cover the
// typical "north / south / east / west" cluster for any given city name.
const maxStationCandidates = 10

// photonEndpoint is komoot's hosted Photon — an Elasticsearch-backed OSM
// geocoder that returns sub-second results suitable for typeahead. Overpass
// (used elsewhere in this package for routing) does the same name lookup in
// 5–30 s, which is way too slow for a live search box.
const photonEndpoint = "https://photon.komoot.io/api/"

// photonTimeout caps a single station-lookup request. Photon normally
// responds in <1 s; anything past 8 s is a stuck request — fail fast so the
// typeahead UX doesn't lock up.
const photonTimeout = 8 * time.Second

type photonResp struct {
	Features []photonFeature `json:"features"`
}

type photonFeature struct {
	Geometry struct {
		Coordinates [2]float64 `json:"coordinates"` // [lon, lat]
	} `json:"geometry"`
	Properties struct {
		OSMID    int64  `json:"osm_id"`
		OSMType  string `json:"osm_type"`
		OSMKey   string `json:"osm_key"`
		OSMValue string `json:"osm_value"`
		Name     string `json:"name"`
		City     string `json:"city"`
		County   string `json:"county"`
		State    string `json:"state"`
		Country  string `json:"country"`
	} `json:"properties"`
}

// hanVariantReplacer swaps Han characters that Photon indexes as distinct
// codepoints but readers treat as the same character across orthographic
// traditions. OSM follows the formal ROC convention (臺/灣/縣/車/鐵) for
// Taiwan, so a search for the simplified-ish common variant (台/湾/县/车/铁)
// returns zero hits even though the station is right there in the index.
// Used to retry a fallback query when the literal search misses.
var hanVariantReplacer = strings.NewReplacer(
	"臺", "台", "台", "臺",
	"灣", "湾", "湾", "灣",
	"縣", "县", "县", "縣",
	"車", "车", "车", "車",
	"鐵", "铁", "铁", "鐵",
)

// LookupStations returns OSM railway-station candidates matching the query.
// Backed by Photon for typeahead-class latency; results include a human
// "region" string (city / state / country) so picks with identical names —
// "西安北" the station vs "西安北" the bus stop, "Springfield" anywhere — can
// be disambiguated at a glance.
//
// If the literal query returns zero hits and the input contains a Han-variant
// character we know Photon doesn't normalize, a fallback search runs with the
// variants swapped (台東 → 臺東, etc.) so Taiwan stations surface even when
// the user typed the casual form.
func (c *Client) LookupStations(ctx context.Context, name string) ([]Station, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}

	// matched is the query Photon actually responded to — equal to `name`
	// when the literal search hit, or the variant-swapped form when the
	// fallback ran. We sort against this so a Taiwan station's exact match
	// surfaces even when the user typed the casual variant.
	matched := name
	out, err := c.photonStations(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if alt := hanVariantReplacer.Replace(name); alt != name {
			out, err = c.photonStations(ctx, alt)
			if err != nil {
				return nil, err
			}
			matched = alt
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no stations found for %q", name)
	}
	// Exact-name matches first so the obvious pick lands at the top of the
	// dropdown. Photon ranks by relevance, which is usually close but not
	// always exactly this.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name == matched && out[j].Name != matched
	})
	return out, nil
}

// photonStations issues one Photon query against the station index and maps
// the response to our Station shape. Empty result list is not an error here —
// callers use the empty-list signal to decide whether to fall back to a
// variant-swapped retry.
func (c *Client) photonStations(ctx context.Context, q string) ([]Station, error) {
	u, _ := url.Parse(photonEndpoint)
	uq := u.Query()
	uq.Set("q", q)
	uq.Set("osm_tag", "railway:station")
	uq.Set("limit", fmt.Sprintf("%d", maxStationCandidates))
	u.RawQuery = uq.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, photonTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", "open-fog/0.1 (rail-route; github.com/loldotenv/open-fog)")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("photon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("photon %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v photonResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("photon decode: %w", err)
	}
	// Drain so the deferred Close lands on EOF — typeahead fires many station
	// lookups per session, and broken keep-alive churns TLS handshakes.
	_, _ = io.Copy(io.Discard, resp.Body)

	out := make([]Station, 0, len(v.Features))
	for _, f := range v.Features {
		// Photon's osm_tag filter is best-effort — defensively drop anything
		// that didn't actually come back tagged as a station. Both key AND
		// value must match: tram_stop / halt / level_crossing all share
		// railway:* but routing assumes mainline rail.
		if f.Properties.OSMKey != "railway" || f.Properties.OSMValue != "station" {
			continue
		}
		out = append(out, Station{
			Name:      f.Properties.Name,
			Latitude:  f.Geometry.Coordinates[1],
			Longitude: f.Geometry.Coordinates[0],
			OSMID:     f.Properties.OSMID,
			OSMType:   f.Properties.OSMType,
			Region:    formatRegion(f.Properties.City, f.Properties.County, f.Properties.State, f.Properties.Country),
		})
	}
	return out, nil
}

// formatRegion joins the non-empty admin levels Photon returned into a single
// "City, State, Country" string for inline disambiguation in the dropdown.
// County is used only when there's no city (Photon labels HSR stations
// outside city limits this way).
func formatRegion(city, county, state, country string) string {
	parts := make([]string, 0, 3)
	switch {
	case city != "":
		parts = append(parts, city)
	case county != "":
		parts = append(parts, county)
	}
	if state != "" && (len(parts) == 0 || state != parts[0]) {
		parts = append(parts, state)
	}
	if country != "" {
		parts = append(parts, country)
	}
	return strings.Join(parts, ", ")
}

// FetchRailNetwork pulls every mainline railway way in a padded bbox around
// the two stations. `service`-tagged ways (yards, sidings, spurs) are filtered
// out so Dijkstra doesn't shortcut through depot tracks.
func (c *Client) FetchRailNetwork(ctx context.Context, a, b Station) (*overpassResp, error) {
	south := math.Min(a.Latitude, b.Latitude) - bboxPadDeg
	west := math.Min(a.Longitude, b.Longitude) - bboxPadDeg
	north := math.Max(a.Latitude, b.Latitude) + bboxPadDeg
	east := math.Max(a.Longitude, b.Longitude) + bboxPadDeg
	// Overpass [timeout:] is the server-side execution cap; match it to the
	// per-mirror client deadline so the mirror gives up around the same time we
	// do instead of grinding on a query we've already abandoned.
	q := fmt.Sprintf(`[out:json][timeout:%d][bbox:%f,%f,%f,%f];
way["railway"~"^(rail|narrow_gauge)$"][!"service"];
out geom;`, int(overpassPerTry/time.Second), south, west, north, east)
	return c.query(ctx, q)
}

// --- graph / routing ----------------------------------------------------

// nodeKey is a coordinate rounded to coordPrecision dp, used as a hashable
// graph node identity. Two coords closer than ~1 cm collapse to one node.
type nodeKey struct {
	Lat, Lon float64
}

func keyOf(lat, lon float64) nodeKey {
	m := math.Pow10(coordPrecision)
	return nodeKey{
		Lat: math.Round(lat*m) / m,
		Lon: math.Round(lon*m) / m,
	}
}

type edge struct {
	to        nodeKey
	weight    float64
	geom      []Point // segment geometry from `from` to `to` (inclusive)
	highspeed bool    // source way was tagged highspeed=yes (Shinkansen, TGV, …)
}

type graph map[nodeKey][]edge

func haversine(aLat, aLon, bLat, bLon float64) float64 {
	const R = 6371000.0
	lat1 := aLat * math.Pi / 180
	lat2 := bLat * math.Pi / 180
	dLat := (bLat - aLat) * math.Pi / 180
	dLon := (bLon - aLon) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	// h is algebraically in [0,1] but FP rounding can push it slightly above 1
	// for near-antipodal pairs; asin(>1) is NaN, which then poisons Dijkstra.
	if h > 1 {
		h = 1
	}
	return 2 * R * math.Asin(math.Sqrt(h))
}

// buildGraph turns Overpass ways into an undirected weighted graph. After
// per-way edges are added, any two nodes within bridgeToleranceM are
// connected with a zero-distance bridge edge so adjacent OSM ways that don't
// share an exact endpoint coordinate still route across the junction.
func buildGraph(ways *overpassResp) graph {
	g := make(graph)
	for _, el := range ways.Elements {
		if el.Type != "way" || len(el.Geometry) < 2 {
			continue
		}
		hs := el.Tags["highspeed"] == "yes"
		for i := 0; i < len(el.Geometry)-1; i++ {
			a, b := el.Geometry[i], el.Geometry[i+1]
			ka, kb := keyOf(a.Lat, a.Lon), keyOf(b.Lat, b.Lon)
			if ka == kb {
				continue
			}
			d := haversine(a.Lat, a.Lon, b.Lat, b.Lon)
			g[ka] = append(g[ka], edge{
				to: kb, weight: d, highspeed: hs,
				geom: []Point{{Latitude: a.Lat, Longitude: a.Lon}, {Latitude: b.Lat, Longitude: b.Lon}},
			})
			g[kb] = append(g[kb], edge{
				to: ka, weight: d, highspeed: hs,
				geom: []Point{{Latitude: b.Lat, Longitude: b.Lon}, {Latitude: a.Lat, Longitude: a.Lon}},
			})
		}
	}
	bridgeNearby(g)
	return g
}

func bridgeNearby(g graph) {
	cellSizeLat := bridgeToleranceM / 111000.0 // ~degrees latitude
	// Lon meters-per-degree shrinks with cos(lat); using cellSizeLat for both
	// dimensions makes lon-cells too narrow at high latitudes, so the ±1-cell
	// scan misses genuine bridge candidates that are within tolerance on the
	// ground. Enlarge cellSizeLon by 1/cos(maxAbsLat) so a ±1 scan still
	// covers bridgeToleranceM at the highest latitude in the graph. Floor
	// cos at 0.1 (~84°) to keep cells bounded near the poles.
	maxAbsLat := 0.0
	for n := range g {
		if a := math.Abs(n.Lat); a > maxAbsLat {
			maxAbsLat = a
		}
	}
	cellSizeLon := cellSizeLat / math.Max(math.Cos(maxAbsLat*math.Pi/180), 0.1)
	type cell struct{ x, y int }
	// math.Floor (not int conversion) so cells are uniformly sized across
	// negative coords too — Go's int() truncates toward zero, which would
	// make the cell straddling lat=0 (or lon=0) twice as wide and lose
	// genuine bridge candidates near the equator / prime meridian.
	cellOf := func(lat, lon float64) cell {
		return cell{int(math.Floor(lat / cellSizeLat)), int(math.Floor(lon / cellSizeLon))}
	}
	grid := make(map[cell][]nodeKey)
	for n := range g {
		k := cellOf(n.Lat, n.Lon)
		grid[k] = append(grid[k], n)
	}
	for n := range g {
		c := cellOf(n.Lat, n.Lon)
		for dx := -1; dx <= 1; dx++ {
			for dy := -1; dy <= 1; dy++ {
				for _, m := range grid[cell{c.x + dx, c.y + dy}] {
					if m == n {
						continue
					}
					d := haversine(n.Lat, n.Lon, m.Lat, m.Lon)
					// d == 0 between distinct nodeKeys means two ways meet at
					// effectively-the-same point but with sub-rounding float
					// drift — bridge them with zero weight to merge in routing.
					// NaN guards against any future caller that lets bad coords
					// reach the graph.
					if math.IsNaN(d) || d > bridgeToleranceM {
						continue
					}
					if hasEdge(g[n], m) {
						continue
					}
					g[n] = append(g[n], edge{
						to: m, weight: d,
						geom: []Point{{Latitude: n.Lat, Longitude: n.Lon}, {Latitude: m.Lat, Longitude: m.Lon}},
					})
				}
			}
		}
	}
}

func hasEdge(edges []edge, to nodeKey) bool {
	for _, e := range edges {
		if e.to == to {
			return true
		}
	}
	return false
}

// nearestNode returns the graph node closest to (lat, lon). Linear scan; for
// the bbox sizes we use (~tens of thousands of nodes at worst) it's fine.
func (g graph) nearestNode(lat, lon float64) (nodeKey, float64) {
	var best nodeKey
	bestD := math.Inf(1)
	for n := range g {
		d := haversine(lat, lon, n.Lat, n.Lon)
		if d < bestD {
			best, bestD = n, d
		}
	}
	return best, bestD
}

// nodeIsHighspeed reports whether any way through n is a high-speed line.
func (g graph) nodeIsHighspeed(n nodeKey) bool {
	for _, e := range g[n] {
		if e.highspeed {
			return true
		}
	}
	return false
}

// nearestHighspeedNode is nearestNode restricted to nodes on a high-speed line.
// ok is false when the graph has none.
func (g graph) nearestHighspeedNode(lat, lon float64) (best nodeKey, dist float64, ok bool) {
	dist = math.Inf(1)
	for n := range g {
		if !g.nodeIsHighspeed(n) {
			continue
		}
		if d := haversine(lat, lon, n.Lat, n.Lon); d < dist {
			best, dist, ok = n, d, true
		}
	}
	return best, dist, ok
}

// snapStation picks the graph node a station boards from. With preferHS on it
// snaps to the nearest high-speed node when one is in range, so the route can
// actually get onto the high-speed line; otherwise (or when none is near) it
// snaps to the nearest node overall.
func (g graph) snapStation(lat, lon float64, preferHS bool) (nodeKey, float64) {
	if preferHS {
		if n, d, ok := g.nearestHighspeedNode(lat, lon); ok && d <= maxSnapM {
			return n, d
		}
	}
	return g.nearestNode(lat, lon)
}

// edgeCost is the routing cost of traversing e: its true length, discounted
// when preferHS is on and the edge is a high-speed line. The reported journey
// distance uses e.weight directly, never this.
func edgeCost(e edge, preferHS bool) float64 {
	if preferHS && e.highspeed {
		return e.weight * highspeedDiscount
	}
	return e.weight
}

// --- Dijkstra (min-heap) ---

type pqItem struct {
	node  nodeKey
	dist  float64
	index int
}

type pq []*pqItem

func (p pq) Len() int            { return len(p) }
func (p pq) Less(i, j int) bool  { return p[i].dist < p[j].dist }
func (p pq) Swap(i, j int)       { p[i], p[j] = p[j], p[i]; p[i].index = i; p[j].index = j }
func (p *pq) Push(x any)         { it := x.(*pqItem); it.index = len(*p); *p = append(*p, it) }
func (p *pq) Pop() any           { old := *p; n := len(old); it := old[n-1]; *p = old[:n-1]; return it }

type prevEdge struct {
	from nodeKey
	geom []Point
}

// dijkstra routes from start to end, preferring a physically realistic train
// path. It first runs the turn-aware search (which forbids sharp turns at
// nodes); if that genuinely finds no path under the angle constraint, it falls
// back to the plain shortest path so a routable pair is never reported as
// disconnected. The fallback can reintroduce the straight "jumps" turn-aware
// routing removes, but only on the rare graph where the sole physical
// connection requires a move our angle cap rejects.
func dijkstra(g graph, start, end nodeKey, preferHS bool) ([]Point, float64, error) {
	coords, total, err := dijkstraTurnAware(g, start, end, preferHS)
	if err == nil {
		return coords, total, nil
	}
	if errors.Is(err, errTurnNoPath) {
		return dijkstraPlain(g, start, end, preferHS)
	}
	return nil, 0, err
}

func dijkstraPlain(g graph, start, end nodeKey, preferHS bool) ([]Point, float64, error) {
	dist := map[nodeKey]float64{start: 0}
	prev := map[nodeKey]prevEdge{}
	open := &pq{}
	heap.Init(open)
	heap.Push(open, &pqItem{node: start, dist: 0})
	for open.Len() > 0 {
		cur := heap.Pop(open).(*pqItem)
		if cur.node == end {
			break
		}
		if cur.dist > dist[cur.node] {
			continue
		}
		for _, e := range g[cur.node] {
			nd := cur.dist + edgeCost(e, preferHS)
			if old, ok := dist[e.to]; !ok || nd < old {
				dist[e.to] = nd
				prev[e.to] = prevEdge{from: cur.node, geom: e.geom}
				heap.Push(open, &pqItem{node: e.to, dist: nd})
			}
		}
	}
	if start == end {
		return nil, 0, errors.New("origin and destination resolve to the same rail node — pick stations further apart")
	}
	if _, ok := prev[end]; !ok {
		return nil, 0, errors.New("no rail path found between stations (graph disconnected — try stations closer to mapped rail, or check OSM coverage)")
	}
	// Walk back, collect segment chunks, reverse, stitch into a single
	// coordinate list. By construction each chunk's first point is the same
	// graph node as the previous chunk's last point (the junction), so we
	// always drop the first point of every chunk after the first. That dedup
	// is keyed on graph identity, not float equality: two OSM ways meeting
	// at slightly-different unrounded coords collapse to one nodeKey via
	// keyOf, but their geom slices retain the originals — comparing those
	// floats with `==` would fail and duplicate the junction.
	var chunks [][]Point
	cur := end
	for {
		p, ok := prev[cur]
		if !ok {
			break
		}
		chunks = append(chunks, p.geom)
		cur = p.from
	}
	// chunks are end→start; reverse.
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	var coords []Point
	for i, ch := range chunks {
		if i == 0 {
			coords = append(coords, ch...)
		} else if len(ch) > 1 {
			coords = append(coords, ch[1:]...)
		}
	}
	// dist[end] is the routing cost, which the high-speed discount distorts;
	// report the true track length measured off the stitched geometry instead.
	total := 0.0
	for i := 1; i < len(coords); i++ {
		total += ptDist(coords[i-1], coords[i])
	}
	return coords, total, nil
}

// --- turn-aware (edge-based) routing ---

// errTurnNoPath signals that the turn-restricted search found no route, so the
// caller should fall back to plain shortest path. Distinct from a genuinely
// disconnected graph (reported by dijkstraPlain) so the right error surfaces.
var errTurnNoPath = errors.New("rail: no path under turn constraints")

// edgeState is a search state for turn-aware routing: a node together with the
// node we arrived from. The pair encodes arrival direction, which a plain
// node-keyed state cannot — and turn cost depends on the (incoming, outgoing)
// edge pair, not just the node.
type edgeState struct {
	at, from nodeKey
}

type turnPrev struct {
	from edgeState
	geom []Point
	seed bool // boarding edge at the origin — has no predecessor state
}

type turnPQItem struct {
	state  edgeState
	inBear float64 // heading of travel arriving at state.at, degrees
	cost   float64 // path cost incl. turn penalties (drives the heap)
	meters float64 // true track length (penalty-free) for the reported distance
	index  int
}

type turnPQ []*turnPQItem

func (p turnPQ) Len() int           { return len(p) }
func (p turnPQ) Less(i, j int) bool { return p[i].cost < p[j].cost }
func (p turnPQ) Swap(i, j int)      { p[i], p[j] = p[j], p[i]; p[i].index = i; p[j].index = j }
func (p *turnPQ) Push(x any)        { it := x.(*turnPQItem); it.index = len(*p); *p = append(*p, it) }
func (p *turnPQ) Pop() any          { old := *p; n := len(old); it := old[n-1]; *p = old[:n-1]; return it }

// bearing is the heading from a to b in degrees, using a local equirectangular
// projection (longitude scaled by cos(mean lat)). Great-circle initial bearing
// would be marginally more correct, but at the sub-kilometre segment lengths we
// compare turns over, the planar heading is accurate to well under a degree.
func bearing(a, b Point) float64 {
	dLat := b.Latitude - a.Latitude
	dLon := (b.Longitude - a.Longitude) * math.Cos((a.Latitude+b.Latitude)/2*math.Pi/180)
	return math.Atan2(dLon, dLat) * 180 / math.Pi
}

// edgeStartBearing is the heading leaving the edge's first node (the departure
// direction at a turn); edgeEndBearing is the heading arriving at its last node.
func edgeStartBearing(e edge) float64 {
	if len(e.geom) < 2 {
		return 0
	}
	return bearing(e.geom[0], e.geom[1])
}

func edgeEndBearing(e edge) float64 {
	n := len(e.geom)
	if n < 2 {
		return 0
	}
	return bearing(e.geom[n-2], e.geom[n-1])
}

// turnDeviation is the absolute heading change in degrees [0,180] between
// arriving along inBear and departing along outBear: 0 is straight through,
// 180 is a full reversal.
func turnDeviation(inBear, outBear float64) float64 {
	d := math.Mod(outBear-inBear+540, 360) - 180
	return math.Abs(d)
}

// dijkstraTurnAware finds the lowest-cost path that never turns sharper than
// maxTurnDeg at any node. Cost is track length plus a small per-degree turn
// penalty (turnPenaltyM) that breaks ties toward the straighter continuation.
// Returns errTurnNoPath if the constraint admits no route.
func dijkstraTurnAware(g graph, start, end nodeKey, preferHS bool) ([]Point, float64, error) {
	if start == end {
		return nil, 0, errors.New("origin and destination resolve to the same rail node — pick stations further apart")
	}
	dist := map[edgeState]float64{}
	prev := map[edgeState]turnPrev{}
	open := &turnPQ{}
	heap.Init(open)

	// Seed: every edge leaving start is a valid boarding move — no turn
	// constraint applies to the first edge.
	for _, e := range g[start] {
		st := edgeState{at: e.to, from: start}
		c := edgeCost(e, preferHS)
		if old, ok := dist[st]; ok && old <= c {
			continue
		}
		dist[st] = c
		prev[st] = turnPrev{geom: e.geom, seed: true}
		heap.Push(open, &turnPQItem{state: st, inBear: edgeEndBearing(e), cost: c, meters: e.weight})
	}

	for open.Len() > 0 {
		cur := heap.Pop(open).(*turnPQItem)
		if cur.cost > dist[cur.state] {
			continue
		}
		if cur.state.at == end {
			return stitchTurnPath(prev, cur.state), cur.meters, nil
		}
		for _, e := range g[cur.state.at] {
			dev := turnDeviation(cur.inBear, edgeStartBearing(e))
			if dev > maxTurnDeg {
				continue // physically impossible turn (crossing, reversal, hop)
			}
			st := edgeState{at: e.to, from: cur.state.at}
			cost := cur.cost + edgeCost(e, preferHS) + dev*turnPenaltyM
			if old, ok := dist[st]; ok && old <= cost {
				continue
			}
			dist[st] = cost
			prev[st] = turnPrev{from: cur.state, geom: e.geom}
			heap.Push(open, &turnPQItem{
				state:  st,
				inBear: edgeEndBearing(e),
				cost:   cost,
				meters: cur.meters + e.weight,
			})
		}
	}
	return nil, 0, errTurnNoPath
}

// stitchTurnPath walks the prev chain from the goal state back to the boarding
// edge and concatenates the segment geometries, dropping the shared junction
// point between consecutive chunks (same dedup rationale as dijkstraPlain:
// keyed on graph identity, not float equality).
func stitchTurnPath(prev map[edgeState]turnPrev, goal edgeState) []Point {
	var chunks [][]Point
	st := goal
	for {
		p := prev[st]
		chunks = append(chunks, p.geom)
		if p.seed {
			break
		}
		st = p.from
	}
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	var coords []Point
	for i, ch := range chunks {
		if i == 0 {
			coords = append(coords, ch...)
		} else if len(ch) > 1 {
			coords = append(coords, ch[1:]...)
		}
	}
	return coords
}

// --- top-level orchestrator ---

// synthDuration is the placeholder span for the synthesized gx:Track
// timestamps. Fog of World ignores the actual values; this just needs to be
// non-zero so consecutive `<when>` entries are monotonically increasing.
const synthDuration = 120 * time.Minute

// GenerateFromCoords routes between two pre-resolved stations and emits a
// Fog of World-compatible KML. The HTTP layer hands in stations the user has
// already confirmed via /api/rail/stations, so this never re-geocodes.
//
// `start` is the journey UTC start time — used only as the first `<when>`
// inside the gx:Track. Fog of World ignores it.
func (c *Client) GenerateFromCoords(ctx context.Context, src, dst Station, start time.Time, preferHS bool) (*Result, error) {
	ways, err := c.FetchRailNetwork(ctx, src, dst)
	if err != nil {
		return nil, err
	}
	g := buildGraph(ways)
	if len(g) == 0 {
		return nil, errors.New("no rail ways found in bbox — OSM coverage may be missing here")
	}
	srcNode, srcSnap := g.snapStation(src.Latitude, src.Longitude, preferHS)
	dstNode, dstSnap := g.snapStation(dst.Latitude, dst.Longitude, preferHS)
	if srcSnap > maxSnapM || dstSnap > maxSnapM {
		return nil, fmt.Errorf("station too far from mapped rail (start %.0f m, end %.0f m) — bbox may not contain a connecting line", srcSnap, dstSnap)
	}
	if srcNode == dstNode {
		return nil, fmt.Errorf("origin and destination snap to the same rail node (%q and %q are too close to distinguish on OSM rail)", src.Name, dst.Name)
	}
	coords, total, err := dijkstra(g, srcNode, dstNode, preferHS)
	if err != nil {
		return nil, err
	}
	coords = smooth(coords, densifyTargetM)
	timestamps := synthTimestamps(len(coords), start, synthDuration)
	for i := range coords {
		coords[i].Timestamp = timestamps[i]
	}
	kml, filename, docName := buildKML(src, dst, coords, start)
	return &Result{
		KML:         kml,
		Filename:    filename,
		DocName:     docName,
		Callsign:    src.Name + "-" + dst.Name,
		Origin:      src,
		Destination: dst,
		Track:       coords,
		DistanceKM:  total / 1000,
	}, nil
}

// smooth resamples a routed polyline with a centripetal Catmull-Rom spline
// (catmullAlpha = 0.5), rounding the polygonal corners left by stitched OSM
// ways — and the small jogs at junctions — without ever looping back on the
// track (centripetal is the only parameterisation proven free of cusps and
// self-intersections). It subsumes the old linear densify: each segment is
// sampled so the chord between samples is <= targetM (the curve between them
// runs marginally longer — a few tens of metres on the sharpest bends), which
// keeps every ~1.1 km Fog of World tile along the route covered.
//
// The spline passes exactly through the first and last points (the snapped
// stations) via reflected boundary tangents. Collinear input stays collinear:
// a straight OSM tunnel/viaduct chord is densified but never bent, because OSM
// has no intermediate geometry there to curve toward — recovering it needs a
// data source this package doesn't use.
func smooth(coords []Point, targetM float64) []Point {
	pts := dedupeConsecutive(coords)
	if len(pts) < 3 || targetM <= 0 {
		// 0/1/2 distinct points: nothing to curve. Linear fill still meets the
		// tile-spacing guarantee (e.g. a 2-point route or a bare tunnel chord).
		return densifyLinear(pts, targetM)
	}
	out := make([]Point, 0, len(pts))
	out = append(out, pts[0])
	for i := 0; i+1 < len(pts); i++ {
		p1, p2 := pts[i], pts[i+1]
		// Reflect the endpoint tangents so the boundary knots are non-zero
		// (clamping by duplication would divide by zero) while the curve still
		// interpolates p1 and p2 exactly.
		p0 := reflect(p2, p1)
		if i > 0 {
			p0 = pts[i-1]
		}
		p3 := reflect(p1, p2)
		if i+2 < len(pts) {
			p3 = pts[i+2]
		}
		out = append(out, catmullRomSegment(p0, p1, p2, p3, targetM)...)
	}
	return out
}

// reflect mirrors `from` across `pivot` (pivot + (pivot - from)), giving a
// phantom control point collinear with the boundary segment.
func reflect(from, pivot Point) Point {
	return Point{
		Latitude:  2*pivot.Latitude - from.Latitude,
		Longitude: 2*pivot.Longitude - from.Longitude,
	}
}

// catmullRomSegment samples the centripetal Catmull-Rom curve between p1 and p2
// (with neighbours p0, p3) at <= targetM spacing. The returned slice excludes
// p1 and includes p2, so callers append it after the running point.
func catmullRomSegment(p0, p1, p2, p3 Point, targetM float64) []Point {
	t0 := 0.0
	t1 := t0 + math.Pow(ptDist(p0, p1), catmullAlpha)
	t2 := t1 + math.Pow(ptDist(p1, p2), catmullAlpha)
	t3 := t2 + math.Pow(ptDist(p2, p3), catmullAlpha)
	// Coincident control points collapse a knot interval; fall back to a
	// straight fill rather than divide by zero.
	if t1 == t0 || t2 == t1 || t3 == t2 {
		return densifyLinear([]Point{p1, p2}, targetM)[1:]
	}
	n := int(math.Ceil(ptDist(p1, p2) / targetM))
	if n < 1 {
		n = 1
	}
	out := make([]Point, 0, n)
	for k := 1; k <= n; k++ {
		t := t1 + (t2-t1)*float64(k)/float64(n)
		out = append(out, catmullRomAt(p0, p1, p2, p3, t0, t1, t2, t3, t))
	}
	return out
}

// catmullRomAt evaluates the curve at parameter t using the Barry-Goldman
// pyramidal form, interpolating latitude and longitude independently.
func catmullRomAt(p0, p1, p2, p3 Point, t0, t1, t2, t3, t float64) Point {
	a1 := lerpPt(p0, p1, (t1-t)/(t1-t0))
	a2 := lerpPt(p1, p2, (t2-t)/(t2-t1))
	a3 := lerpPt(p2, p3, (t3-t)/(t3-t2))
	b1 := lerpPt(a1, a2, (t2-t)/(t2-t0))
	b2 := lerpPt(a2, a3, (t3-t)/(t3-t1))
	return lerpPt(b1, b2, (t2-t)/(t2-t1))
}

// lerpPt returns wa*a + (1-wa)*b component-wise.
func lerpPt(a, b Point, wa float64) Point {
	wb := 1 - wa
	return Point{
		Latitude:  wa*a.Latitude + wb*b.Latitude,
		Longitude: wa*a.Longitude + wb*b.Longitude,
	}
}

func ptDist(a, b Point) float64 {
	return haversine(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
}

// dedupeConsecutive drops points identical to their predecessor so smoothing
// never sees a zero-length segment (which would collapse a Catmull-Rom knot).
func dedupeConsecutive(coords []Point) []Point {
	if len(coords) < 2 {
		return coords
	}
	out := make([]Point, 0, len(coords))
	out = append(out, coords[0])
	for i := 1; i < len(coords); i++ {
		last := out[len(out)-1]
		if coords[i].Latitude == last.Latitude && coords[i].Longitude == last.Longitude {
			continue
		}
		out = append(out, coords[i])
	}
	return out
}

// densifyLinear inserts linearly-interpolated points between any consecutive
// pair whose great-circle distance exceeds targetM. Used for the degenerate
// (<3-point) cases smooth() can't curve, and as the zero-knot fallback inside a
// segment. Cheap linear interp in lat/lon is accurate to centimeters at the
// sub-kilometer step sizes here, well below KML's `%.5f` (~1 m) write precision.
func densifyLinear(coords []Point, targetM float64) []Point {
	if len(coords) < 2 || targetM <= 0 {
		return coords
	}
	out := make([]Point, 0, len(coords))
	out = append(out, coords[0])
	for i := 1; i < len(coords); i++ {
		a, b := coords[i-1], coords[i]
		d := haversine(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
		if d > targetM {
			steps := int(math.Ceil(d / targetM))
			for s := 1; s < steps; s++ {
				t := float64(s) / float64(steps)
				out = append(out, Point{
					Latitude:  a.Latitude + (b.Latitude-a.Latitude)*t,
					Longitude: a.Longitude + (b.Longitude-a.Longitude)*t,
				})
			}
		}
		out = append(out, b)
	}
	return out
}

func synthTimestamps(n int, start time.Time, duration time.Duration) []int64 {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []int64{start.Unix()}
	}
	step := duration.Seconds() / float64(n-1)
	// gx:Track requires <when> entries to be strictly monotonic. Int64-
	// truncating base+i*step collides on consecutive indices whenever
	// step < 1 s (e.g., a long densified route exceeding synthDuration in
	// sample count). Clamping step to 1 s/sample lets timestamps run past
	// synthDuration but stay monotonic — Fog of World ignores the absolute
	// values, so the extension is harmless.
	if step < 1 {
		step = 1
	}
	out := make([]int64, n)
	base := float64(start.Unix())
	for i := range out {
		out[i] = int64(base + float64(i)*step)
	}
	return out
}
