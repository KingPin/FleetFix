package network

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Expectations measured against the v1 parser, as elsewhere in this package.

// cfg fills in the empty lists so a case only has to state what it has.
func cfg(nameservers, search, options []string) ResolverConfig {
	c := ResolverConfig{Nameservers: []string{}, Search: []string{}, Options: []string{}}
	if nameservers != nil {
		c.Nameservers = nameservers
	}
	if search != nil {
		c.Search = search
	}
	if options != nil {
		c.Options = options
	}
	return c
}

func TestParseResolvConfFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		want ResolverConfig
	}{
		{
			name: "classic", file: "resolv_conf/classic.txt",
			want: cfg(
				[]string{"10.0.0.53", "10.0.1.53", "1.1.1.1"},
				[]string{"corp.internal", "internal"},
				[]string{"timeout:2", "attempts:3"},
			),
		},
		{
			// Both directives set the search list, so the later one wins.
			name: "domain then search", file: "resolv_conf/domain_then_search.txt",
			want: cfg([]string{"10.0.0.53"}, []string{"a.internal", "b.internal"}, nil),
		},
		{
			// A ';' comment line, and a '#' comment after a directive.
			name: "legacy domain", file: "resolv_conf/legacy_domain.txt",
			want: cfg([]string{"10.0.0.53"}, []string{"corp.internal"}, nil),
		},
		{
			name: "nameserver without an address", file: "resolv_conf/nameserver_without_address.txt",
			want: cfg([]string{"10.0.0.1"}, nil, nil),
		},
		{
			name: "stub among several", file: "resolv_conf/stub_among_several.txt",
			want: cfg([]string{SystemdStub, "1.1.1.1"}, nil, nil),
		},
		{
			name: "systemd stub", file: "resolv_conf/systemd_stub.txt",
			want: cfg([]string{SystemdStub}, []string{"."}, []string{"edns0", "trust-ad"}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseResolvConf(fixture.Text(t, tt.file), "")
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseResolvConf() mismatch (-want +got):\n%s", diff)
			}
			checkResolver(t, got)
		})
	}
}

func TestParseResolvConfDirectives(t *testing.T) {
	tests := []struct {
		name string
		text string
		want ResolverConfig
	}{
		{name: "empty", text: "", want: cfg(nil, nil, nil)},
		{name: "blank and comment lines", text: "\n # x\n ; y\n\n", want: cfg(nil, nil, nil)},
		{
			// tuple(args) of nothing: a bare search clears the list.
			name: "bare search clears the search list", text: "search a b\nsearch",
			want: cfg(nil, nil, nil),
		},
		{
			// ...but a bare domain has no argument to normalise, so it is ignored
			// and the earlier search stands.
			name: "bare domain leaves the search list alone", text: "search a b\ndomain",
			want: cfg(nil, []string{"a", "b"}, nil),
		},
		{name: "domain takes only its first word", text: "domain a b c", want: cfg(nil, []string{"a"}, nil)},
		{
			name: "nameserver takes only its first word", text: "nameserver 1.1.1.1 2.2.2.2",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			// options accumulates across lines rather than overwriting.
			name: "options accumulate", text: "options a\noptions b c\noptions",
			want: cfg(nil, nil, []string{"a", "b", "c"}),
		},
		{
			name: "keywords are case sensitive", text: "Nameserver 1.1.1.1\nSEARCH x",
			want: cfg(nil, nil, nil),
		},
		{
			name: "unknown keyword", text: "sortlist 1.2.3.4\nnameserver 1.1.1.1",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			name: "bare keywords", text: "nameserver\nsearch\noptions\ndomain",
			want: cfg(nil, nil, nil),
		},
		{
			// Duplicates are kept: the file said to ask the same server twice.
			name: "duplicate nameservers", text: "nameserver 1.1.1.1\nnameserver 1.1.1.1",
			want: cfg([]string{"1.1.1.1", "1.1.1.1"}, nil, nil),
		},
		{
			name: "trailing hash comment", text: "nameserver 1.1.1.1 # primary",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			name: "hash comment without a space", text: "nameserver 1.1.1.1#primary",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			// '#' is cut before ';', so a ';' inside a '#' comment is already gone
			// -- and a '#' inside a ';' comment likewise never gets its own turn.
			name: "hash then semicolon", text: "nameserver 1.1.1.1 # a ; b",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			name: "semicolon then hash", text: "nameserver 1.1.1.1 ; a # b",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			// A ';' needs no space in front of it either, so it truncates a value.
			name: "semicolon inside an argument", text: "search a;b",
			want: cfg(nil, []string{"a"}, nil),
		},
		{
			name: "surrounding whitespace", text: "   nameserver 1.1.1.1   ",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{name: "tab separated", text: "nameserver\t1.1.1.1", want: cfg([]string{"1.1.1.1"}, nil, nil)},
		{
			// Python's split() is Unicode-aware.
			name: "no-break space separated", text: "nameserver\u00a01.1.1.1",
			want: cfg([]string{"1.1.1.1"}, nil, nil),
		},
		{
			// Python's splitlines boundaries, not just \n.
			name: "exotic line boundaries",
			text: "nameserver 1.1.1.1\x0cnameserver 2.2.2.2\x1cnameserver 3.3.3.3" +
				"\u0085nameserver 4.4.4.4\u2028nameserver 5.5.5.5",
			want: cfg([]string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}, nil, nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseResolvConf(tt.text, "")
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseResolvConf() mismatch (-want +got):\n%s", diff)
			}
			checkResolver(t, got)
		})
	}
}

func TestParseResolvConfRecordsItsSource(t *testing.T) {
	got := ParseResolvConf("nameserver 1.1.1.1", "/etc/resolv.conf")
	if got.Source != "/etc/resolv.conf" {
		t.Errorf("Source = %q, want the path the caller passed", got.Source)
	}
}

func TestResolverConfigStubResolver(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "the stub alone", text: "nameserver " + SystemdStub, want: true},
		{name: "no nameservers", text: ""},
		{name: "a real nameserver", text: "nameserver 1.1.1.1"},
		{name: "the stub and a real one", text: "nameserver " + SystemdStub + "\nnameserver 1.1.1.1"},
		// Equality against a one-element tuple, so listing the stub twice is not
		// the stub-only case.
		{name: "the stub twice", text: "nameserver " + SystemdStub + "\nnameserver " + SystemdStub},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseResolvConf(tt.text, "").StubResolver(); got != tt.want {
				t.Errorf("StubResolver() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseResolvConfMarshalsEmptyCollectionsAsLists(t *testing.T) {
	b, err := json.Marshal(ParseResolvConf("", ""))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const want = `{"nameservers":[],"search":[],"options":[],"source":""}`
	if string(b) != want {
		t.Errorf("json.Marshal() = %s, want %s", b, want)
	}
}

// checkResolver asserts what holds of every result regardless of input.
func checkResolver(t *testing.T, got ResolverConfig) {
	t.Helper()
	if got.Nameservers == nil || got.Search == nil || got.Options == nil {
		t.Errorf("config %+v has a nil collection, want empty ones", got)
	}
	// Every element came out of a whitespace split of a comment-stripped line.
	for _, group := range [][]string{got.Nameservers, got.Search, got.Options} {
		for _, v := range group {
			if v == "" {
				t.Errorf("config %+v has an empty entry", got)
			}
			if strings.ContainsAny(v, "#;") {
				t.Errorf("config %+v kept a comment character in %q", got, v)
			}
			if strings.IndexFunc(v, pytext.IsSpace) >= 0 {
				t.Errorf("config %+v has whitespace in %q", got, v)
			}
		}
	}
}

func FuzzParseResolvConf(f *testing.F) {
	for _, name := range []string{
		"resolv_conf/classic.txt", "resolv_conf/domain_then_search.txt",
		"resolv_conf/legacy_domain.txt", "resolv_conf/nameserver_without_address.txt",
		"resolv_conf/stub_among_several.txt", "resolv_conf/systemd_stub.txt",
	} {
		f.Add(fixture.Text(f, name))
	}

	f.Fuzz(func(t *testing.T, text string) {
		got := ParseResolvConf(text, "src")
		checkResolver(t, got)
		if got.Source != "src" {
			t.Errorf("Source = %q, want it carried through untouched", got.Source)
		}
	})
}
