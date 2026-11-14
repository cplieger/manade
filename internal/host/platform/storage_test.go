package platform

import (
	"errors"
	"strings"
	"testing"
)

const magicZFS, magicExt4 = 0x2fc12fc1, 0xef53

func seams(fss string, magic int64, overlay error) *storageSeams {
	return &storageSeams{
		filesystems: func() (string, error) { return fss, nil },
		fsType:      func(string) (int64, error) { return magic, nil },
		overlay:     func(string) error { return overlay },
	}
}

const (
	withOverlay    = "nodev\tproc\nnodev\toverlay\n\text4\n"
	withoutOverlay = "nodev\tproc\n\tbtrfs\n\text4\n"
)

func TestPickStorage(t *testing.T) {
	renameFails := errors.New("no whiteout for the rename")
	cases := map[string]struct {
		s       *storageSeams
		who     For
		want    Driver
		refused bool
		names   []string
	}{
		"overlay2OnZFS":       {s: seams(withOverlay, magicZFS, nil), who: ForHost, want: Overlay2},
		"overlay2OnBtrfs":     {s: seams(withOverlay, magicBtrfs, nil), who: ForHost, want: Overlay2},
		"btrfsWithoutOverlay": {s: seams(withoutOverlay, magicBtrfs, nil), who: ForHost, want: Btrfs, names: []string{"lists no overlay"}},
		"btrfsWhenTestFails":  {s: seams(withOverlay, magicBtrfs, renameFails), who: ForHost, want: Btrfs, names: []string{"rename"}},
		"oldZFSHostRefused": {
			s: seams(withOverlay, magicZFS, renameFails), who: ForHost, refused: true,
			names: []string{"zfs", "rename", "OpenZFS 2.2", "ext4", "ftype=1"},
		},
		"oldZFSJobVFS":         {s: seams(withOverlay, magicZFS, renameFails), who: ForJob, want: VFS, names: []string{"zfs", "rename"}},
		"noOverlayExt4Refused": {s: seams(withoutOverlay, magicExt4, nil), who: ForHost, refused: true, names: []string{"ext4", "lists no overlay"}},
		"overlayfsHostRefused": {s: seams(withOverlay, magicOverlay, nil), who: ForHost, refused: true, names: []string{"overlayfs", "volume"}},
		"overlayfsJobVFS":      {s: seams(withOverlay, magicOverlay, nil), who: ForJob, want: VFS, names: []string{"overlayfs"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := pickStorage(tc.s, "/var/lib/docker", tc.who)
			text := got.Why
			if tc.refused {
				if !errors.Is(err, ErrNoStorageDriver) {
					t.Fatalf("err = %v, want ErrNoStorageDriver", err)
				}
				text = err.Error()
			} else if err != nil || got.Driver != tc.want {
				t.Fatalf("pick = %+v, %v, want %s", got, err, tc.want)
			}
			for _, n := range tc.names {
				if !strings.Contains(text, n) {
					t.Errorf("%q does not name %q", text, n)
				}
			}
		})
	}
}

func TestPickStorageSkipsTheTestOverlayWhereItCannotHold(t *testing.T) {
	for name, s := range map[string]*storageSeams{
		"overlayfs":       seams(withOverlay, magicOverlay, nil),
		"overlayUnlisted": seams(withoutOverlay, magicBtrfs, nil),
	} {
		t.Run(name, func(t *testing.T) {
			s.overlay = func(string) error { t.Error("the test overlay was mounted"); return nil }
			_, _ = pickStorage(s, "/var/lib/docker", ForJob)
		})
	}
}

func TestListsFilesystem(t *testing.T) {
	if !listsFilesystem(withOverlay, "overlay") {
		t.Error("overlay not found in a nodev line")
	}
	if listsFilesystem("nodev\toverlayfs-x\n", "overlay") {
		t.Error("a longer name matched")
	}
}

func TestProjectQuota(t *testing.T) {
	ms, err := parseMountinfo("" +
		"1 0 0:1 / / rw - overlay overlay rw\n" +
		"2 1 0:2 / /var/lib/docker rw - xfs /dev/sdb rw,attr2,inode64,prjquota\n" +
		"3 1 0:3 / /data rw - xfs /dev/sdc rw,attr2,inode64,noquota\n" +
		"4 1 0:4 / /data/sub rw - xfs /dev/sdd rw,pquota\n")
	if err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{"/var/lib/docker": true, "/var/lib/docker/x": true, "/data": false, "/data/sub/y": true, "/var/lib": false} {
		if got := ProjectQuota(ms, dir); got != want {
			t.Errorf("ProjectQuota(%s) = %t, want %t", dir, got, want)
		}
	}
}

func TestDataUsage(t *testing.T) {
	u, err := DataUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.Size == 0 || u.Free > u.Size {
		t.Errorf("usage %+v, want a size holding the free bytes", u)
	}
}
