// Package kml converts a Flightradar24 playback record into a Fog of World-
// compatible KML — gx:Track with pre-takeoff / post-landing taxi points
// (altitude == 0) stripped, which is what Fog of World's importer needs.
package kml

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loldotenv/open-fog/internal/fr24"
)

// Result is what the HTTP layer hands back to the browser.
type Result struct {
	KML         string         `json:"kml"`
	Filename    string         `json:"filename"`
	DocName     string         `json:"docName"`
	Callsign    string         `json:"callsign"`
	Origin      Airport        `json:"origin"`
	Destination Airport        `json:"destination"`
	Airborne    []TrackPoint   `json:"airborne"`
}

// Airport / TrackPoint are projected versions of fr24's types — flat enough for
// the frontend to consume without re-implementing the FR24 schema.
type Airport struct {
	Name      string  `json:"name"`
	IATA      string  `json:"iata,omitempty"`
	ICAO      string  `json:"icao"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	City      string  `json:"city,omitempty"`
}

// code returns the airport's IATA if available, else ICAO. Used for filename
// and document-name display where IATA is preferred for readability.
func (a Airport) code() string {
	if a.IATA != "" {
		return a.IATA
	}
	return a.ICAO
}

type TrackPoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  int     `json:"altitude"` // meters
	Timestamp int64   `json:"timestamp"`
}

var months = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

func Convert(pb *fr24.PlaybackJSON) (*Result, error) {
	f := pb.Result.Response.Data.Flight
	if len(f.Track) == 0 {
		return nil, errors.New("no track points in playback")
	}

	// Strip pre-takeoff and post-landing taxi (altitude.meters > 0)
	// — Fog of World silently drops the track otherwise.
	airborne := make([]TrackPoint, 0, len(f.Track))
	for _, p := range f.Track {
		if p.Altitude.Meters > 0 {
			airborne = append(airborne, TrackPoint{
				Latitude:  p.Latitude,
				Longitude: p.Longitude,
				Altitude:  p.Altitude.Meters,
				Timestamp: p.Timestamp,
			})
		}
	}
	if len(airborne) == 0 {
		return nil, errors.New("no airborne points (altitude > 0) in track")
	}

	// Prefer the IATA-form flight number (3U6311) — that's what users type and
	// what's on the boarding pass. Fall back to ATC callsign (CSC6311) only if
	// FR24 didn't fill the number field.
	callsign := f.Identification.Number.Default
	if callsign == "" && f.Identification.Callsign != nil {
		callsign = *f.Identification.Callsign
	}
	origin := Airport{
		Name: f.Airport.Origin.Name, IATA: f.Airport.Origin.Code.IATA, ICAO: f.Airport.Origin.Code.ICAO,
		Latitude: f.Airport.Origin.Position.Latitude, Longitude: f.Airport.Origin.Position.Longitude,
		City: f.Airport.Origin.Position.Region.City,
	}
	dest := Airport{
		Name: f.Airport.Destination.Name, IATA: f.Airport.Destination.Code.IATA, ICAO: f.Airport.Destination.Code.ICAO,
		Latitude: f.Airport.Destination.Position.Latitude, Longitude: f.Airport.Destination.Position.Longitude,
		City: f.Airport.Destination.Position.Region.City,
	}

	firstTs := time.Unix(airborne[0].Timestamp, 0).UTC()
	dateLong := fmt.Sprintf("%02d-%s-%d", firstTs.Day(), months[int(firstTs.Month())-1], firstTs.Year())
	dateShort := firstTs.Format("20060102")

	// `FlightAware ✈` prefix is a brand-match check by Fog of World's importer
	// — without it the file imports cleanly but uncovers no tiles. Keep this in
	// sync with internal/rail/kml.go.
	docName := fmt.Sprintf("FlightAware ✈ %s %s (%s-%s)",
		callsign, dateLong, origin.code(), dest.code())

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<kml xmlns="http://www.opengis.net/kml/2.2" xmlns:gx="http://www.google.com/kml/ext/2.2">` + "\n")
	sb.WriteString("<Document>\n")
	fmt.Fprintf(&sb, "    <name>%s</name>\n", xmlEscape(docName))
	writeAirport(&sb, f.Airport.Origin)
	writeAirport(&sb, f.Airport.Destination)
	sb.WriteString("    <Placemark>\n")
	fmt.Fprintf(&sb, "        <name>%s</name>\n", xmlEscape(callsign))
	fmt.Fprintf(&sb, "        <description>%s - %s</description>\n", xmlEscape(origin.code()), xmlEscape(dest.code()))
	sb.WriteString("        <gx:Track>\n")
	sb.WriteString("            <extrude>1</extrude>\n")
	sb.WriteString("            <tessellate>1</tessellate>\n")
	sb.WriteString("            <altitudeMode>absolute</altitudeMode>\n")
	for _, p := range airborne {
		fmt.Fprintf(&sb, "            <when>%s</when>\n",
			time.Unix(p.Timestamp, 0).UTC().Format("2006-01-02T15:04:05Z"))
	}
	for i, p := range airborne {
		fmt.Fprintf(&sb, "            <gx:coord>%.5f %.5f %d</gx:coord>",
			p.Longitude, p.Latitude, p.Altitude)
		if i < len(airborne)-1 {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n        </gx:Track>\n")
	sb.WriteString("    </Placemark>\n")
	sb.WriteString("</Document>\n")
	sb.WriteString("</kml>\n")

	return &Result{
		KML:         sb.String(),
		Filename:    fmt.Sprintf("%s_%s_%s_%s.kml", callsign, origin.code(), dest.code(), dateShort),
		DocName:     docName,
		Callsign:    callsign,
		Origin:      origin,
		Destination: dest,
		Airborne:    airborne,
	}, nil
}

func writeAirport(sb *strings.Builder, a fr24.Airport) {
	code := a.Code.IATA
	if code == "" {
		code = a.Code.ICAO
	}
	desc := a.Name
	if a.Position.Region.City != "" {
		desc = a.Name + " <br> " + a.Position.Region.City
	}
	sb.WriteString("    <Placemark>\n")
	fmt.Fprintf(sb, "        <name>%s Airport</name>\n", code)
	fmt.Fprintf(sb, "        <description><![CDATA[%s]]></description>\n", cdataSafe(desc))
	sb.WriteString("        <Point>\n")
	// %.5f to match track-point precision and avoid scientific notation for
	// coordinates near the prime meridian / equator.
	fmt.Fprintf(sb, "            <coordinates>%.5f,%.5f,0</coordinates>\n",
		a.Position.Longitude, a.Position.Latitude)
	sb.WriteString("        </Point>\n")
	sb.WriteString("    </Placemark>\n")
}

// cdataSafe replaces any "]]>" sequences in s with a split that preserves the
// literal characters but doesn't terminate the enclosing CDATA section.
func cdataSafe(s string) string {
	return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
}

func xmlEscape(s string) string {
	var sb strings.Builder
	_ = xml.EscapeText(&sb, []byte(s))
	return sb.String()
}
