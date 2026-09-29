package cli

import (
	"reflect"
	"testing"
)

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"512": 512, "64k": 64 << 10, "64K": 64 << 10, "128m": 128 << 20, "128MiB": 128 << 20,
		"1g": 1 << 30, "1.5g": 3 << 29, "2gb": 2 << 30,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "m", "-1m", "ten"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0B", 1023: "1023B", 1024: "1.0KiB", 64 << 20: "64.0MiB", 3 << 29: "1.5GiB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("%d = %s, want %s", n, got, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	t.Setenv("FROM_HOST", "h")
	got := mergeEnv([]string{"PATH=/bin", "A=1"}, []string{"A=2", "B=3", "FROM_HOST"})
	want := []string{"PATH=/bin", "A=2", "B=3", "FROM_HOST=h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestParseVolumes(t *testing.T) {
	dir := t.TempDir()
	b, err := parseVolumes([]string{dir + ":/data:ro", dir + ":/rw"})
	if err != nil || len(b) != 2 || !b[0].ReadOnly || b[1].ReadOnly || b[0].Target != "/data" {
		t.Fatalf("%+v %v", b, err)
	}
	for _, bad := range []string{"nocolon", dir + ":relative", dir + ":/x:maybe", "/does/not/exist:/x"} {
		if _, err := parseVolumes([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
