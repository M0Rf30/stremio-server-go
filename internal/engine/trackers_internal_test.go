package engine

import (
	"reflect"
	"testing"
)

// TestAnnounceableTrackers guards the fix for the panic that anacrolix raises
// when a ws/wss tracker reaches its regular announce dispatcher (which happens
// once WebTorrent is disabled and the websocket announcer is gone):
// tracker.NewClient → ErrBadScheme → panicif.Err.
func TestAnnounceableTrackers(t *testing.T) {
	in := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"http://t.example/announce",
		"https://t.example/announce",
		"udp4://t4.example:6969/announce",
		"udp6://t6.example:6969/announce",
		"  WSS://tracker.openwebtorrent.com  ", // case + whitespace
		"ws://tracker.example",
		"dht://nodes", // unknown scheme must always be dropped
		"",            // empty must always be dropped
	}

	// WebTorrent disabled: ws/wss and unknown schemes dropped.
	gotOff := announceableTrackers(in, false)
	wantOff := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"http://t.example/announce",
		"https://t.example/announce",
		"udp4://t4.example:6969/announce",
		"udp6://t6.example:6969/announce",
	}
	if !reflect.DeepEqual(gotOff, wantOff) {
		t.Errorf("allowWS=false:\n got  %q\n want %q", gotOff, wantOff)
	}

	// WebTorrent enabled: ws/wss kept (verbatim), unknown still dropped.
	gotOn := announceableTrackers(in, true)
	wantOn := append(append([]string{}, wantOff...),
		"  WSS://tracker.openwebtorrent.com  ", "ws://tracker.example")
	if !reflect.DeepEqual(gotOn, wantOn) {
		t.Errorf("allowWS=true:\n got  %q\n want %q", gotOn, wantOn)
	}
}

// TestAnnounceableTiers verifies per-tier filtering drops tiers left empty.
func TestAnnounceableTiers(t *testing.T) {
	in := [][]string{
		{"wss://only.websocket"},                         // -> dropped (empty after filter)
		{"udp://keep.me:1337/announce", "wss://drop.me"}, // -> {udp...}
		{"http://a/announce", "https://b/announce"},      // -> both kept
	}
	got := announceableTiers(in, false)
	want := [][]string{
		{"udp://keep.me:1337/announce"},
		{"http://a/announce", "https://b/announce"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("announceableTiers:\n got  %q\n want %q", got, want)
	}
}
