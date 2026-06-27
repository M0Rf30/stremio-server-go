package ftpstream

import "testing"

// TestParseFTPURL verifies URL component extraction from ftp:// and ftps:// URLs.
// No network connection is used.
func TestParseFTPURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		addr    string
		host    string
		user    string
		pass    string
		path    string
		isTLS   bool
		wantErr bool
	}{
		{
			name:   "full credentials with non-default port",
			rawURL: "ftp://alice:secret@media.example.com:2121/videos/movie.mkv",
			addr:   "media.example.com:2121",
			host:   "media.example.com",
			user:   "alice",
			pass:   "secret",
			path:   "/videos/movie.mkv",
			isTLS:  false,
		},
		{
			name:   "anonymous defaults with default port",
			rawURL: "ftp://ftp.example.org/pub/file.mp4",
			addr:   "ftp.example.org:21",
			host:   "ftp.example.org",
			user:   "anonymous",
			pass:   "",
			path:   "/pub/file.mp4",
			isTLS:  false,
		},
		{
			name:   "ftps enables TLS flag",
			rawURL: "ftps://secure.example.com/data/film.mkv",
			addr:   "secure.example.com:21",
			host:   "secure.example.com",
			user:   "anonymous",
			pass:   "",
			path:   "/data/film.mkv",
			isTLS:  true,
		},
		{
			name:   "ftps with credentials and port",
			rawURL: "ftps://bob:pass@ftps.host:990/share/clip.avi",
			addr:   "ftps.host:990",
			host:   "ftps.host",
			user:   "bob",
			pass:   "pass",
			path:   "/share/clip.avi",
			isTLS:  true,
		},
		{
			name:   "user without password",
			rawURL: "ftp://carol@host.local:21/files/video.ts",
			addr:   "host.local:21",
			host:   "host.local",
			user:   "carol",
			pass:   "",
			path:   "/files/video.ts",
			isTLS:  false,
		},
		{
			name:    "unsupported http scheme",
			rawURL:  "http://host/file.mp4",
			wantErr: true,
		},
		{
			name:    "unsupported https scheme",
			rawURL:  "https://host/file.mp4",
			wantErr: true,
		},
		{
			name:    "missing host",
			rawURL:  "ftp:///path/to/file.mkv",
			wantErr: true,
		},
		{
			name:    "malformed URL",
			rawURL:  "://bad",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFTPURL(tc.rawURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFTPURL(%q): expected error, got nil", tc.rawURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFTPURL(%q): unexpected error: %v", tc.rawURL, err)
			}
			if got.addr != tc.addr {
				t.Errorf("addr: got %q, want %q", got.addr, tc.addr)
			}
			if got.host != tc.host {
				t.Errorf("host: got %q, want %q", got.host, tc.host)
			}
			if got.user != tc.user {
				t.Errorf("user: got %q, want %q", got.user, tc.user)
			}
			if got.pass != tc.pass {
				t.Errorf("pass: got %q, want %q", got.pass, tc.pass)
			}
			if got.path != tc.path {
				t.Errorf("path: got %q, want %q", got.path, tc.path)
			}
			if got.tls != tc.isTLS {
				t.Errorf("tls: got %v, want %v", got.tls, tc.isTLS)
			}
		})
	}
}
