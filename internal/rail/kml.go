package rail

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

var monthShort = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// buildKML emits the Fog of World-compatible KML. The `FlightAware ✈` prefix
// in the document <name> is required — without it the file imports but
// uncovers no tiles. The filename does not need the prefix. Keep in sync
// with internal/kml/kml.go.
func buildKML(origin, dest Station, coords []Point, start time.Time) (kml, filename, docName string) {
	callsign := origin.Name + "-" + dest.Name
	dateLong := fmt.Sprintf("%02d-%s-%d", start.UTC().Day(), monthShort[int(start.UTC().Month())-1], start.UTC().Year())
	dateISO := start.UTC().Format("2006-01-02")
	docName = fmt.Sprintf("FlightAware ✈ %s %s (%s-%s)", callsign, dateLong, origin.Name, dest.Name)

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<kml xmlns="http://www.opengis.net/kml/2.2" xmlns:gx="http://www.google.com/kml/ext/2.2">` + "\n")
	sb.WriteString("<Document>\n")
	fmt.Fprintf(&sb, "    <name>%s</name>\n", xmlEscape(docName))
	writeStation(&sb, origin)
	writeStation(&sb, dest)
	sb.WriteString("    <Placemark>\n")
	fmt.Fprintf(&sb, "        <name>%s</name>\n", xmlEscape(callsign))
	fmt.Fprintf(&sb, "        <description>%s - %s</description>\n", xmlEscape(origin.Name), xmlEscape(dest.Name))
	sb.WriteString("        <gx:Track>\n")
	sb.WriteString("            <extrude>1</extrude>\n")
	sb.WriteString("            <tessellate>1</tessellate>\n")
	sb.WriteString("            <altitudeMode>clampToGround</altitudeMode>\n")
	for _, p := range coords {
		fmt.Fprintf(&sb, "            <when>%s</when>\n",
			time.Unix(p.Timestamp, 0).UTC().Format("2006-01-02T15:04:05Z"))
	}
	for i, p := range coords {
		fmt.Fprintf(&sb, "            <gx:coord>%.5f %.5f 0</gx:coord>", p.Longitude, p.Latitude)
		if i < len(coords)-1 {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n        </gx:Track>\n")
	sb.WriteString("    </Placemark>\n")
	sb.WriteString("</Document>\n")
	sb.WriteString("</kml>\n")

	filename = fmt.Sprintf("%s_%s.kml", callsign, dateISO)
	return sb.String(), filename, docName
}

func writeStation(sb *strings.Builder, s Station) {
	sb.WriteString("    <Placemark>\n")
	fmt.Fprintf(sb, "        <name>%s</name>\n", xmlEscape(s.Name))
	fmt.Fprintf(sb, "        <description><![CDATA[%s (rail)]]></description>\n", cdataSafe(s.Name))
	sb.WriteString("        <Point>\n")
	fmt.Fprintf(sb, "            <coordinates>%.5f,%.5f,0</coordinates>\n", s.Longitude, s.Latitude)
	sb.WriteString("        </Point>\n")
	sb.WriteString("    </Placemark>\n")
}

func cdataSafe(s string) string {
	return strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
}

func xmlEscape(s string) string {
	var sb strings.Builder
	_ = xml.EscapeText(&sb, []byte(s))
	return sb.String()
}
