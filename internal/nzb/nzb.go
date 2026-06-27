// Package nzb provides NZB XML parsing, yEnc decoding, and an NNTP client for
// assembling Usenet files from their individual article segments.
package nzb

import (
	"encoding/xml"
	"regexp"
	"sort"
	"strings"
)

// ---- NZB XML schema types (unexported) -------------------------------------

type nzbRoot struct {
	XMLName xml.Name  `xml:"nzb"`
	Files   []nzbFile `xml:"file"`
}

type nzbFile struct {
	Subject  string       `xml:"subject,attr"`
	Segments []nzbSegment `xml:"segments>segment"`
}

type nzbSegment struct {
	MessageID string `xml:",chardata"`
	Bytes     int64  `xml:"bytes,attr"`
	Number    int    `xml:"number,attr"`
}

// ---- Public types ----------------------------------------------------------

// File represents a single file described in an NZB, with its constituent
// download segments. Size is the sum of segment byte counts.
type File struct {
	Subject  string
	Name     string
	Segments []Segment
	Size     int64
}

// Segment is one NNTP article that constitutes part of a File.
type Segment struct {
	MessageID string
	Bytes     int64
	Number    int
}

// quotedNameRE extracts a filename from a quoted token in a subject line.
// A quoted filename is expected to contain at least one dot (e.g. "movie.mkv").
var quotedNameRE = regexp.MustCompile(`"([^"]*\.[^"]*)"`)

// Parse decodes NZB XML and returns a slice of File values, each with segments
// sorted ascending by Number and Size equal to the sum of segment byte counts.
func Parse(data []byte) ([]File, error) {
	var root nzbRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	files := make([]File, 0, len(root.Files))
	for _, f := range root.Files {
		file := File{Subject: f.Subject}

		// Derive name: prefer quoted "filename.ext" in subject, else whole subject.
		if m := quotedNameRE.FindStringSubmatch(f.Subject); len(m) > 1 {
			file.Name = m[1]
		} else {
			file.Name = strings.TrimSpace(f.Subject)
		}

		file.Segments = make([]Segment, len(f.Segments))
		for i, seg := range f.Segments {
			file.Segments[i] = Segment{
				MessageID: strings.TrimSpace(seg.MessageID),
				Bytes:     seg.Bytes,
				Number:    seg.Number,
			}
			file.Size += seg.Bytes
		}
		sort.Slice(file.Segments, func(i, j int) bool {
			return file.Segments[i].Number < file.Segments[j].Number
		})

		files = append(files, file)
	}
	return files, nil
}
