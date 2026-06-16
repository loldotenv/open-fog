package rail

import (
	"errors"
	"math"
	"testing"
)

// wayElem builds an Overpass way element from [lat, lon] pairs, so tests can
// assemble synthetic rail networks and feed them through the real buildGraph.
func wayElem(coords ...[2]float64) overpassElem {
	g := make([]overpassGeomPt, len(coords))
	for i, c := range coords {
		g[i] = overpassGeomPt{Lat: c[0], Lon: c[1]}
	}
	return overpassElem{Type: "way", Geometry: g}
}

func testGraph(elems ...overpassElem) graph {
	return buildGraph(&overpassResp{Elements: elems})
}

// maxTurn is the sharpest heading change (degrees) between any three
// consecutive points — the metric that distinguishes a smooth train path from
// one that "jumps" at junctions.
func maxTurn(coords []Point) float64 {
	worst := 0.0
	for i := 1; i+1 < len(coords); i++ {
		d := turnDeviation(bearing(coords[i-1], coords[i]), bearing(coords[i], coords[i+1]))
		if d > worst {
			worst = d
		}
	}
	return worst
}

func containsCoord(coords []Point, lat, lon, tolDeg float64) bool {
	for _, p := range coords {
		if math.Abs(p.Latitude-lat) < tolDeg && math.Abs(p.Longitude-lon) < tolDeg {
			return true
		}
	}
	return false
}

// semicircleArc returns a south-bulging half-circle from (0,0) to (0,0.040),
// sampled every 15° so every internal turn is ~15° — a fully turn-legal but
// longer alternative to a short, sharp shortcut.
func semicircleArc() [][2]float64 {
	const r = 0.020
	var arc [][2]float64
	for d := 180; d <= 360; d += 15 {
		rad := float64(d) * math.Pi / 180
		arc = append(arc, [2]float64{r * math.Sin(rad), 0.020 + r*math.Cos(rad)})
	}
	// Pin the endpoints to exact coords so they share graph nodes with the
	// shortcut way (float sin/cos leaves ~1e-18 dust otherwise).
	arc[0] = [2]float64{0, 0}
	arc[len(arc)-1] = [2]float64{0, 0.040}
	return arc
}

// TestTurnAwareAvoidsSharpShortcut is the core regression: between a short path
// that needs a ~74° turn and a longer all-gentle arc, plain shortest-path takes
// the sharp shortcut (the visible "jump"), while turn-aware routing takes the
// smooth arc. Both connect the same endpoints.
func TestTurnAwareAvoidsSharpShortcut(t *testing.T) {
	sharp := wayElem([2]float64{0, 0}, [2]float64{0.015, 0.020}, [2]float64{0, 0.040})
	arc := wayElem(semicircleArc()...)
	g := testGraph(sharp, arc)

	start, end := keyOf(0, 0), keyOf(0, 0.040)

	// Plain routing prefers the shorter sharp path — proving the graph genuinely
	// tempts the jump, so the turn-aware result below isn't a no-op.
	plain, _, err := dijkstraPlain(g, start, end, false)
	if err != nil {
		t.Fatalf("plain routing failed: %v", err)
	}
	if mt := maxTurn(plain); mt <= maxTurnDeg {
		t.Fatalf("plain path turned at most %.1f° — expected it to take the sharp (>%.0f°) shortcut", mt, maxTurnDeg)
	}
	if !containsCoord(plain, 0.015, 0.020, 1e-4) {
		t.Fatal("expected plain path to pass through the sharp shortcut vertex")
	}

	// Turn-aware routing must reject the shortcut and follow the arc.
	got, _, err := dijkstra(g, start, end, false)
	if err != nil {
		t.Fatalf("turn-aware routing failed: %v", err)
	}
	if mt := maxTurn(got); mt > maxTurnDeg+1 {
		t.Fatalf("turn-aware path turned %.1f° — sharper than the %.0f° cap", mt, maxTurnDeg)
	}
	if containsCoord(got, 0.015, 0.020, 1e-4) {
		t.Fatal("turn-aware path went through the sharp shortcut vertex — it should detour via the arc")
	}
	if !coordNear(got[0], 0, 0) || !coordNear(got[len(got)-1], 0, 0.040) {
		t.Fatalf("path endpoints %v..%v don't match start/end", got[0], got[len(got)-1])
	}
}

// TestTurnAwareFallback: when the only physical route requires an impossible
// turn (a 90° spur off a through line), the wrapper must still return a path via
// the plain fallback rather than reporting the pair as unroutable.
func TestTurnAwareFallback(t *testing.T) {
	through := wayElem([2]float64{0, 0}, [2]float64{0, 0.01}, [2]float64{0, 0.02}, [2]float64{0, 0.03})
	spur := wayElem([2]float64{0, 0.01}, [2]float64{0.02, 0.01}) // 90° off the line
	g := testGraph(through, spur)

	start, dest := keyOf(0, 0), keyOf(0.02, 0.01)

	if _, _, err := dijkstraTurnAware(g, start, dest, false); !errors.Is(err, errTurnNoPath) {
		t.Fatalf("turn-aware should find no legal path to a 90° spur, got err=%v", err)
	}
	got, _, err := dijkstra(g, start, dest, false)
	if err != nil {
		t.Fatalf("wrapper should fall back to plain routing, got %v", err)
	}
	if !coordNear(got[len(got)-1], 0.02, 0.01) {
		t.Fatalf("fallback path didn't reach the spur end: %v", got[len(got)-1])
	}
}

func coordNear(p Point, lat, lon float64) bool {
	return math.Abs(p.Latitude-lat) < 1e-6 && math.Abs(p.Longitude-lon) < 1e-6
}

// hsWay is wayElem tagged highspeed=yes, modelling a Shinkansen/TGV alignment.
func hsWay(coords ...[2]float64) overpassElem {
	e := wayElem(coords...)
	e.Tags = map[string]string{"highspeed": "yes"}
	return e
}

func minLat(coords []Point) float64 {
	m := coords[0].Latitude
	for _, p := range coords {
		if p.Latitude < m {
			m = p.Latitude
		}
	}
	return m
}

// TestHighspeedPreference: a short straight conventional line and a longer
// high-speed arc both connect S→E. Without the preference the router takes the
// shorter conventional line; with it, the cost discount makes it follow the
// longer high-speed arc instead — exactly the Tokyo→Kyoto "why isn't it the
// Shinkansen" case.
func TestHighspeedPreference(t *testing.T) {
	conventional := wayElem([2]float64{0, 0}, [2]float64{0, 0.020}, [2]float64{0, 0.040}) // straight, ~4.4 km
	highspeed := hsWay(semicircleArc()...)                                                // arc, ~7 km, highspeed=yes
	g := testGraph(conventional, highspeed)
	start, end := keyOf(0, 0), keyOf(0, 0.040)

	// Preference off → shortest path is the straight conventional line (lat≈0,
	// no southern bulge).
	off, _, err := dijkstra(g, start, end, false)
	if err != nil {
		t.Fatalf("routing (preferHS=false) failed: %v", err)
	}
	if ml := minLat(off); ml < -0.001 {
		t.Fatalf("preferHS=false took the high-speed arc (min lat %.4f) — should stay on the shorter conventional line", ml)
	}

	// Preference on → the high-speed arc wins despite being longer; its southern
	// bulge (down to lat −0.020) must appear in the path.
	on, _, err := dijkstra(g, start, end, true)
	if err != nil {
		t.Fatalf("routing (preferHS=true) failed: %v", err)
	}
	if ml := minLat(on); ml > -0.015 {
		t.Fatalf("preferHS=true did not follow the high-speed arc (min lat %.4f) — expected the southern bulge", ml)
	}
	if !coordNear(on[0], 0, 0) || !coordNear(on[len(on)-1], 0, 0.040) {
		t.Fatalf("high-speed route endpoints %v..%v don't match start/end", on[0], on[len(on)-1])
	}
}

// TestSnapStationPrefersHighspeed: boarding the high-speed line needs the start
// to snap to a high-speed node even when a conventional node is closer —
// otherwise the cost discount is unreachable (the connector would be a
// turn-forbidden near-perpendicular hop).
func TestSnapStationPrefersHighspeed(t *testing.T) {
	conventional := wayElem([2]float64{0, 0}, [2]float64{0, 0.01})       // node (0,0) sits on the query point
	highspeed := hsWay([2]float64{0.002, 0}, [2]float64{0.002, 0.01})    // ~222 m away, highspeed
	g := testGraph(conventional, highspeed)

	if n, _ := g.snapStation(0, 0, true); !g.nodeIsHighspeed(n) {
		t.Fatalf("preferHS snap picked a non-high-speed node %v despite a high-speed node in range", n)
	}
	if n, _ := g.snapStation(0, 0, false); g.nodeIsHighspeed(n) {
		t.Fatalf("plain snap picked a high-speed node %v instead of the nearer conventional one", n)
	}
}

// TestSmooth exercises the centripetal Catmull-Rom resampler end-to-end:
// corners get rounded, tile spacing is kept, endpoints are exact, and the
// tunnel-chord guarantee (collinear in → collinear out) holds.
func TestSmooth(t *testing.T) {
	const target = densifyTargetM
	const maxGap = target * 1.5 // arc between samples runs slightly over the chord

	assertSpacingAndFinite := func(t *testing.T, out []Point) {
		t.Helper()
		for i, p := range out {
			if math.IsNaN(p.Latitude) || math.IsNaN(p.Longitude) ||
				math.IsInf(p.Latitude, 0) || math.IsInf(p.Longitude, 0) {
				t.Fatalf("non-finite point at %d: %v", i, p)
			}
			if i == 0 {
				continue
			}
			if g := ptDist(out[i-1], out[i]); g > maxGap {
				t.Fatalf("gap %.0f m at index %d exceeds %.0f m — densify guarantee broken", g, i, maxGap)
			}
		}
	}

	t.Run("rounds a right-angle corner", func(t *testing.T) {
		in := []Point{{Latitude: 0, Longitude: 0}, {Latitude: 0, Longitude: 0.02}, {Latitude: 0.02, Longitude: 0.02}}
		out := smooth(in, target)
		if len(out) <= len(in) {
			t.Fatalf("smoothing added no points (%d -> %d)", len(in), len(out))
		}
		// The input has a 90° vertex; smoothing must distribute it so no three
		// consecutive output points reproduce a near-90° kink.
		if mt := maxTurn(out); mt > 85 {
			t.Fatalf("corner not rounded: max turn %.1f° still near the original 90°", mt)
		}
		if !coordNear(out[0], 0, 0) || !coordNear(out[len(out)-1], 0.02, 0.02) {
			t.Fatalf("endpoints not preserved: %v..%v", out[0], out[len(out)-1])
		}
		assertSpacingAndFinite(t, out)
	})

	t.Run("keeps a straight tunnel chord straight", func(t *testing.T) {
		in := []Point{{Latitude: 0, Longitude: 0}, {Latitude: 0, Longitude: 0.01}, {Latitude: 0, Longitude: 0.02}, {Latitude: 0, Longitude: 0.03}}
		out := smooth(in, target)
		for _, p := range out {
			if math.Abs(p.Latitude) > 1e-9 {
				t.Fatalf("collinear input bent off the line: lat %.3e", p.Latitude)
			}
		}
		assertSpacingAndFinite(t, out)
	})

	t.Run("densifies a bare two-point chord", func(t *testing.T) {
		in := []Point{{Latitude: 0, Longitude: 0}, {Latitude: 0, Longitude: 0.03}}
		out := smooth(in, target)
		if len(out) <= 2 {
			t.Fatalf("two-point chord not densified: %d points", len(out))
		}
		if !coordNear(out[0], 0, 0) || !coordNear(out[len(out)-1], 0, 0.03) {
			t.Fatalf("endpoints not preserved: %v..%v", out[0], out[len(out)-1])
		}
		assertSpacingAndFinite(t, out)
	})
}
