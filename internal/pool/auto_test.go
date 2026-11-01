package pool

import (
	"errors"
	"testing"
	"time"
)

func autoPool(t *testing.T) *AutoAdmission {
	return &AutoAdmission{Pool: slice(t, "0.00", 1<<30, 0), DiskPath: "/host-docker", ReadDisk: func(string) (Disk, error) {
		return Disk{Total: 100 << 30, Available: 40 << 30, Files: 1000, FilesFree: 900}, nil
	}}
}

func TestAutoAdmissionPacesColdStartsAndPreservesHeadroom(t *testing.T) {
	a := autoPool(t)
	now := time.Now()
	if ok, _ := a.Admit(0, now); !ok {
		t.Fatal("first attempt blocked")
	}
	a.Started(now)
	for _, elapsed := range []time.Duration{0, time.Second, ColdStartInterval - time.Nanosecond} {
		if ok, _ := a.Admit(1, now.Add(elapsed)); ok {
			t.Fatal("cold burst admitted", elapsed)
		}
	}
	if ok, reason := a.Admit(1, now.Add(ColdStartInterval)); !ok {
		t.Fatal(reason)
	}
	// A compiler grows late, after warmup. Its observed working set raises the
	// estimate, rather than permitting a second equally expensive build.
	write(t, a.Pool.Dir, map[string]string{"memory.current": "5368709120"})
	a.Observe(1)
	if ok, _ := a.Admit(1, now.Add(2*ColdStartInterval)); ok {
		t.Fatal("new build without headroom admitted")
	}
	write(t, a.Pool.Dir, map[string]string{"memory.current": "1073741824"})
	if ok, _ := a.Admit(1, now.Add(2*ColdStartInterval)); ok {
		t.Fatal("observed peak lost when current usage dropped")
	}
	if ok, _ := a.Admit(0, now.Add(2*ColdStartInterval)); !ok {
		t.Fatal("history deadlocked an empty pool")
	}
}

func TestAutoAdmissionDiskHysteresisAndReadErrors(t *testing.T) {
	a := autoPool(t)
	d := Disk{Total: 100 << 30, Available: 9 << 30, Files: 1000, FilesFree: 900}
	a.ReadDisk = func(string) (Disk, error) { return d, nil }
	now := time.Now()
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("low disk admitted")
	}
	d.Available = 12 << 30
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("disk gate flapped below recovery watermark")
	}
	d.Available = 16 << 30
	if ok, reason := a.Admit(0, now); !ok {
		t.Fatal(reason)
	}
	d.FilesFree = 40
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("inode exhaustion admitted")
	}
	d.FilesFree = 70
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("inode gate flapped")
	}
	d.FilesFree = 100
	if ok, reason := a.Admit(0, now); !ok {
		t.Fatal(reason)
	}
	a.ReadDisk = func(string) (Disk, error) { return Disk{}, errors.New("missing mount") }
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("missing disk mount admitted")
	}
}

func TestReadDisk(t *testing.T) {
	if d, err := ReadDisk(t.TempDir()); err != nil || d.Total == 0 || d.Available > d.Total {
		t.Fatal(d, err)
	}
	if _, err := ReadDisk(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing path accepted")
	}
}
