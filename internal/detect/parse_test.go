package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		wantTot, wantAvl uint64
	}{
		{
			name:    "typical",
			in:      "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\n",
			wantTot: 16384000 * 1024,
			wantAvl: 8192000 * 1024,
		},
		{
			name:    "missing available",
			in:      "MemTotal:       2048 kB\nMemFree:          512 kB\n",
			wantTot: 2048 * 1024,
			wantAvl: 0,
		},
		{
			name:    "empty",
			in:      "",
			wantTot: 0,
			wantAvl: 0,
		},
		{
			name:    "garbage lines ignored",
			in:      "this is not meminfo\nMemTotal:\nMemAvailable: 100 kB\n",
			wantTot: 0,
			wantAvl: 100 * 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tot, avl := parseMeminfo(strings.NewReader(tt.in))
			if tot != tt.wantTot || avl != tt.wantAvl {
				t.Fatalf("parseMeminfo = (%d, %d), want (%d, %d)", tot, avl, tt.wantTot, tt.wantAvl)
			}
		})
	}
}

func TestParseCPUInfo(t *testing.T) {
	// Two physical cores, two threads each (4 logical) on one socket.
	smt := `processor	: 0
model name	: Test CPU @ 3.0GHz
physical id	: 0
core id		: 0

processor	: 1
model name	: Test CPU @ 3.0GHz
physical id	: 0
core id		: 1

processor	: 2
model name	: Test CPU @ 3.0GHz
physical id	: 0
core id		: 0

processor	: 3
model name	: Test CPU @ 3.0GHz
physical id	: 0
core id		: 1
`
	tests := []struct {
		name      string
		in        string
		wantModel string
		wantCores int
	}{
		{name: "smt 2 physical", in: smt, wantModel: "Test CPU @ 3.0GHz", wantCores: 2},
		{
			name:      "dual socket single core each",
			in:        "model name : A\nphysical id : 0\ncore id : 0\n\nmodel name : A\nphysical id : 1\ncore id : 0\n",
			wantModel: "A",
			wantCores: 2,
		},
		{
			name:      "no trailing blank line still counts last record",
			in:        "model name : Solo\nphysical id : 0\ncore id : 0",
			wantModel: "Solo",
			wantCores: 1,
		},
		{name: "empty", in: "", wantModel: "", wantCores: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, cores := parseCPUInfo(strings.NewReader(tt.in))
			if model != tt.wantModel || cores != tt.wantCores {
				t.Fatalf("parseCPUInfo = (%q, %d), want (%q, %d)", model, cores, tt.wantModel, tt.wantCores)
			}
		})
	}
}

func TestParseCgroupCPUMax(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"max 100000", 0},
		{"100000 100000", 1},
		{"50000 100000", 0.5},
		{"200000 100000\n", 2},
		{"max", 0},
		{"", 0},
		{"abc 100000", 0},
		{"100000 0", 0},
	}
	for _, tt := range tests {
		if got := parseCgroupCPUMax(tt.in); got != tt.want {
			t.Errorf("parseCgroupCPUMax(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseCgroupMemMax(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{"max", 0},
		{"max\n", 0},
		{"2147483648", 2147483648},
		{"  1024  \n", 1024},
		{"notanumber", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := parseCgroupMemMax(tt.in); got != tt.want {
			t.Errorf("parseCgroupMemMax(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestCgroupFilePath(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "unified",
			contents: "0::/system.slice/foo.service\n",
			want:     filepath.Join("/sys/fs/cgroup", "/system.slice/foo.service", "cpu.max"),
		},
		{
			name:     "v1 lines ignored, unified used",
			contents: "12:cpuset:/\n0::/leaf\n",
			want:     filepath.Join("/sys/fs/cgroup", "/leaf", "cpu.max"),
		},
		{name: "no unified line", contents: "1:name=systemd:/foo\n", want: ""},
		{name: "empty", contents: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cgroupFilePath(tt.contents, "cpu.max"); got != tt.want {
				t.Fatalf("cgroupFilePath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadRotationalAt(t *testing.T) {
	mk := func(t *testing.T, devs map[string]string) string {
		root := t.TempDir()
		for dev, rot := range devs {
			qdir := filepath.Join(root, dev, "queue")
			if err := os.MkdirAll(qdir, 0o755); err != nil {
				t.Fatal(err)
			}
			if rot != "" {
				if err := os.WriteFile(filepath.Join(qdir, "rotational"), []byte(rot), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return root
	}

	t.Run("ssd", func(t *testing.T) {
		root := mk(t, map[string]string{"sda": "0\n"})
		got := readRotationalAt(root)
		if got == nil || *got {
			t.Fatalf("want non-nil false, got %v", got)
		}
	})
	t.Run("hdd", func(t *testing.T) {
		root := mk(t, map[string]string{"sda": "1\n"})
		got := readRotationalAt(root)
		if got == nil || !*got {
			t.Fatalf("want non-nil true, got %v", got)
		}
	})
	t.Run("skips pseudo devices", func(t *testing.T) {
		// loop0 has rotational=1 but must be skipped in favor of the real disk.
		root := mk(t, map[string]string{"loop0": "1\n", "ram0": "1\n", "vda": "0\n"})
		got := readRotationalAt(root)
		if got == nil || *got {
			t.Fatalf("want non-nil false (real disk vda), got %v", got)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		if got := readRotationalAt(filepath.Join(t.TempDir(), "nope")); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})
	t.Run("only pseudo devices", func(t *testing.T) {
		root := mk(t, map[string]string{"loop0": "1\n"})
		if got := readRotationalAt(root); got != nil {
			t.Fatalf("want nil (no real disk), got %v", got)
		}
	})
}

func TestSplitField(t *testing.T) {
	k, v, ok := splitField("model name\t: Foo Bar")
	if !ok || k != "model name" || v != "Foo Bar" {
		t.Fatalf("got (%q,%q,%v)", k, v, ok)
	}
	if _, _, ok := splitField("no colon here"); ok {
		t.Fatalf("expected ok=false for line without colon")
	}
}

func TestHelpers_Format(t *testing.T) {
	if got := trimFloat(1.5); got != "1.5" {
		t.Errorf("trimFloat = %q", got)
	}
	if got := humanGiB(2 << 30); got != "2.0 GiB" {
		t.Errorf("humanGiB = %q", got)
	}
}

func TestDetect_SmokeNonNil(t *testing.T) {
	// Detect() runs against the real host; we only assert it returns a sane,
	// populated Profile (OS set, logical cores positive, effective cores set).
	p := Detect()
	if p.OS == "" {
		t.Fatal("OS should be set")
	}
	if p.LogicalCores < 1 {
		t.Fatalf("LogicalCores = %d", p.LogicalCores)
	}
	if p.EffectiveCores <= 0 {
		t.Fatalf("EffectiveCores = %v", p.EffectiveCores)
	}
}
