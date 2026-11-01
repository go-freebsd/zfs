//go:build freebsd

package primitives

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// run executes a command with standard input and returns its output.
func run(t *testing.T, stdin []byte, name string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		err = errors.New(err.Error() + ": " + strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), err
}

// A corrective receive: damaged blocks of a snapshot are rewritten from a
// stream of that snapshot. A pool of its own on one file has no redundancy,
// so the damage is permanent until the stream heals it. zfs receive -c heals
// first as the oracle, then the library heals the same damage again, and
// both refuse a stream of another snapshot.
func TestHealLikeZfs(t *testing.T) {
	c, _ := testClient(t)

	dir := t.TempDir()
	img := filepath.Join(dir, "heal.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	const pool = "gozfsheal"
	if out, err := run(t, nil, "zpool", "create", "-f", "-m", "none", pool, img); err != nil {
		t.Fatalf("zpool create: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		run(t, nil, "zpool", "destroy", "-f", pool)
		run(t, nil, "zpool", "import", "-d", dir, "-f", "-N", pool)
		run(t, nil, "zpool", "destroy", "-f", pool)
	})

	mnt := t.TempDir()
	cli(t, "zfs", "create", "-o", "mountpoint="+mnt, "-o", "compression=off", "-o", "recordsize=128k", pool+"/ds")
	file := filepath.Join(mnt, "data")
	want := make([]byte, 1<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, want, 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "zpool", "sync", pool)
	cli(t, "zfs", "snapshot", pool+"/ds@a")

	stream, err := run(t, nil, "zfs", "send", pool+"/ds@a")
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(file, &st); err != nil {
		t.Fatal(err)
	}

	// a second snapshot, whose stream must not heal the first
	if err := os.WriteFile(filepath.Join(mnt, "other"), []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "zfs", "snapshot", pool+"/ds@b")
	other, err := run(t, nil, "zfs", "send", pool+"/ds@b")
	if err != nil {
		t.Fatal(err)
	}

	// Where the data blocks of the file are: export the pool and ask zdb.
	cli(t, "zpool", "export", pool)
	zdb, err := run(t, nil, "zdb", "-e", "-p", dir, "-ddddd", pool+"/ds", strconv.FormatUint(uint64(st.Ino), 10))
	if err != nil {
		t.Fatalf("zdb: %v\n%s", err, zdb)
	}
	blocks := regexp.MustCompile(`\bL0 0:([0-9a-f]+):([0-9a-f]+)`).FindAllStringSubmatch(string(zdb), -1)
	if len(blocks) < 4 {
		t.Fatalf("zdb shows %d data blocks of the file, want at least 4:\n%s", len(blocks), zdb)
	}
	cli(t, "zpool", "import", "-d", dir, pool)

	// corrupt overwrites the blocks, with the pool exported, and imports it
	// again; the file can then not be read.
	corrupt := func(label string) {
		t.Helper()
		cli(t, "zpool", "export", pool)
		f, err := os.OpenFile(img, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		garbage := bytes.Repeat([]byte{0xA5}, 128<<10)
		for _, b := range blocks[:4] {
			off, _ := strconv.ParseUint(b[1], 16, 64)
			size, _ := strconv.ParseUint(b[2], 16, 64)
			// vdev labels and the boot area take the first 4 MiB
			if _, err := f.WriteAt(garbage[:size], int64(off)+4<<20); err != nil {
				t.Fatal(err)
			}
		}
		f.Close()
		cli(t, "zpool", "import", "-d", dir, pool)
		if _, err := os.ReadFile(file); err == nil {
			t.Fatalf("%s: the damaged file can be read", label)
		}
	}
	healed := func(label string) {
		t.Helper()
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: the file differs after healing", label)
		}
		cli(t, "zpool", "clear", pool)
	}

	corrupt("before zfs receive -c")
	// a stream of another snapshot is refused by the CLI
	if _, err := run(t, other, "zfs", "receive", "-c", pool+"/ds@a"); err == nil {
		t.Error("zfs receive -c accepted the stream of another snapshot")
	}
	if out, err := run(t, stream, "zfs", "receive", "-c", pool+"/ds@a"); err != nil {
		t.Fatalf("zfs receive -c: %v\n%s", err, out)
	}
	healed("zfs receive -c")

	corrupt("before Heal")
	if _, err := c.Receive(context.Background(), bytes.NewReader(other), pool+"/ds@a", &ReceiveOptions{Heal: true}); err == nil {
		t.Error("Heal accepted the stream of another snapshot")
	}
	if _, err := c.Receive(context.Background(), bytes.NewReader(stream), pool+"/ds@a", &ReceiveOptions{Heal: true}); err != nil {
		t.Fatalf("Receive with Heal: %v", err)
	}
	healed("Heal")

	// the pool is clean again
	cli(t, "zpool", "scrub", "-w", pool)
	if st := cli(t, "zpool", "status", "-v", pool); !strings.Contains(st, "No known data errors") {
		t.Errorf("after the scrub:\n%s", st)
	}
}
