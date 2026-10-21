package main

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func sample(id string) disk {
	yes, no := true, false
	i := diskInfo{DeviceIdentifier: id, Internal: &no, SolidState: &yes, WholeDisk: true,
		VirtualOrPhysical: "Physical", TotalSize: 512000000000, MediaName: "SSD"}
	return disk{diskInfo{DeviceIdentifier: id}, i}
}

func TestSelectDisk(t *testing.T) {
	d := sample("disk4")
	unknown := sample("disk6")
	unknown.info.SolidState = nil
	for _, tc := range []struct {
		name      string
		disks     []disk
		requested string
		ok        bool
	}{
		{"unique SSD", []disk{d}, "", true},
		{"multiple SSDs", []disk{d, sample("disk6")}, "", false},
		{"unknown media", []disk{unknown}, "", false},
		{"explicit unknown media", []disk{unknown}, "/dev/disk6", true},
		{"internal absent", []disk{d}, "disk0", false},
		{"partition", []disk{d}, "disk4s1", false},
		{"shell input", []disk{d}, "disk4; reboot", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := selectDisk(tc.disks, tc.requested)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDiscoveryFailsClosed(t *testing.T) {
	for _, mode := range []string{"internal", "unknown", "virtual", "partition", "mismatch", "valid"} {
		t.Run(mode, func(t *testing.T) {
			d := sample("disk4")
			switch mode {
			case "internal":
				yes := true
				d.info.Internal = &yes
			case "unknown":
				d.info.Internal = nil
			case "virtual":
				d.info.VirtualOrPhysical = "Virtual"
			case "partition":
				d.info.WholeDisk = false
			case "mismatch":
				d.info.DeviceIdentifier = "disk0"
			}
			a := app{load: func(args ...string) (diskData, error) {
				if args[0] == "list" {
					return diskData{AllDisksAndPartitions: []diskInfo{d.entry}}, nil
				}
				return diskData{diskInfo: d.info}, nil
			}}
			got, err := a.externalDisks()
			if err != nil || (len(got) == 1) != (mode == "valid") {
				t.Fatalf("disks=%v error=%v", got, err)
			}
		})
	}
}

func TestAPFSMapping(t *testing.T) {
	entry := diskInfo{DeviceIdentifier: "disk4", Partitions: []diskInfo{{DeviceIdentifier: "disk4s2"}}}
	containers := []containerInfo{
		{PhysicalStores: []diskInfo{{DeviceIdentifier: "disk4s2"}}, Volumes: []diskInfo{{DeviceIdentifier: "disk5s1"}}},
		{PhysicalStores: []diskInfo{{DeviceIdentifier: "disk40s2"}}, Volumes: []diskInfo{{DeviceIdentifier: "disk41s1"}}},
	}
	if got := volumeDevices(entry, containers); !reflect.DeepEqual(got, []string{"disk4", "disk4s2", "disk5s1"}) {
		t.Fatal(got)
	}
}

func TestEjectionSafety(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		force, preview, fail, changed bool
		want                          []string
		wantError                     bool
	}{
		{name: "preview", force: true, preview: true},
		{name: "normal", want: []string{"eject"}},
		{name: "busy never forces", fail: true, want: []string{"eject"}, wantError: true},
		{name: "force", force: true, want: []string{"unmountDisk", "eject"}},
		{name: "force failure stops", force: true, fail: true, want: []string{"unmountDisk"}, wantError: true},
		{name: "identity changed", changed: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := sample("disk4")
			var calls []string
			a := app{out: io.Discard}
			a.load = func(args ...string) (diskData, error) {
				switch args[0] {
				case "list":
					return diskData{AllDisksAndPartitions: []diskInfo{d.entry}}, nil
				case "info":
					i := d.info
					if tc.changed {
						i.TotalSize++
					}
					return diskData{diskInfo: i}, nil
				default:
					return diskData{}, nil
				}
			}
			a.run = func(_ []byte, name string, args ...string) ([]byte, error) {
				if name != diskutil {
					t.Fatalf("unexpected command %s", name)
				}
				calls = append(calls, args[0])
				if tc.fail {
					return nil, errors.New("busy")
				}
				return []byte("OK\n"), nil
			}
			err := a.eject(d, tc.force, tc.preview)
			if (err != nil) != tc.wantError || !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls=%v error=%v", calls, err)
			}
		})
	}
}

func TestLoadPlistPreservesSafetyFields(t *testing.T) {
	data, err := loadPlist(func(input []byte, name string, args ...string) ([]byte, error) {
		if name == diskutil {
			return []byte("plist bytes"), nil
		}
		if string(input) != "plist bytes" {
			t.Fatal("plist not passed through stdin")
		}
		return []byte(`{"DeviceIdentifier":"disk4","Internal":false,"WholeDisk":true,"SolidState":true,"TotalSize":512110190592,"VirtualOrPhysical":"Physical"}`), nil
	}, "info", "-plist", "disk4")
	if err != nil || data.Internal == nil || *data.Internal || !data.WholeDisk || data.SolidState == nil || !*data.SolidState || data.TotalSize != 512110190592 {
		t.Fatalf("%+v %v", data, err)
	}
	_, err = loadPlist(func(_ []byte, _ string, _ ...string) ([]byte, error) { return []byte("invalid"), nil })
	if err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatal(err)
	}
}

func TestADBRecovery(t *testing.T) {
	listing := "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n" +
		"adb 3571 dev cwd DIR 1,24 1184 1084700 /Volumes/ssd/sdk/Flutter-3.44.9\n" +
		"adb 3571 dev txt REG 1,24 18844128 564895 " + adbPath + "\n"
	for _, tc := range []struct {
		name, listing, mount                                  string
		killFails, retryFails, changed, diagnoseOnly, preview bool
		want                                                  []string
		wantError                                             bool
	}{
		{name: "stop adb and retry", listing: listing, want: []string{"eject", "kill-server", "eject"}},
		{name: "kill failure stops", listing: listing, killFails: true, want: []string{"eject", "kill-server"}, wantError: true},
		{name: "retry failure stops", listing: listing, retryFails: true, want: []string{"eject", "kill-server", "eject"}, wantError: true},
		{name: "identity changed before retry", listing: listing, changed: true, want: []string{"eject", "kill-server"}, wantError: true},
		{name: "other adb path", listing: strings.ReplaceAll(listing, adbPath, "/usr/local/bin/adb"), want: []string{"eject"}, wantError: true},
		{name: "cwd only", listing: "adb 3571 dev cwd DIR 1,24 1184 1084700 /Volumes/ssd\n", want: []string{"eject"}, wantError: true},
		{name: "other process", listing: strings.ReplaceAll(listing, "adb 3571", "other 3571"), want: []string{"eject"}, wantError: true},
		{name: "other target mount", listing: listing, mount: "/Volumes/other", want: []string{"eject"}, wantError: true},
		{name: "diagnosis never stops adb", listing: listing, diagnoseOnly: true},
		{name: "preview never stops adb", listing: listing, preview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := sample("disk4")
			mount := tc.mount
			if mount == "" {
				mount = "/Volumes/ssd"
			}
			var calls []string
			attempts := 0
			stopped := false
			a := app{out: io.Discard}
			a.load = func(args ...string) (diskData, error) {
				switch args[0] {
				case "list":
					return diskData{AllDisksAndPartitions: []diskInfo{d.entry}}, nil
				case "info":
					i := d.info
					i.MountPoint = mount
					if stopped && tc.changed {
						i.TotalSize++
					}
					return diskData{diskInfo: i}, nil
				default:
					return diskData{}, nil
				}
			}
			a.run = func(_ []byte, name string, args ...string) ([]byte, error) {
				switch name {
				case "/usr/sbin/lsof":
					return []byte(tc.listing), nil
				case adbPath:
					if !reflect.DeepEqual(args, []string{"kill-server"}) {
						t.Fatalf("adb args: %v", args)
					}
					calls = append(calls, "kill-server")
					stopped = true
					if tc.killFails {
						return nil, errors.New("kill failed")
					}
					return nil, nil
				case diskutil:
					if !reflect.DeepEqual(args, []string{"eject", "/dev/disk4"}) {
						t.Fatalf("diskutil args: %v", args)
					}
					calls = append(calls, "eject")
					attempts++
					if attempts == 1 || tc.retryFails {
						return nil, errors.New("busy")
					}
					return nil, nil
				default:
					t.Fatalf("unexpected command: %s", name)
					return nil, nil
				}
			}
			var err error
			if tc.diagnoseOnly {
				a.diagnose(d)
			} else {
				err = a.eject(d, false, tc.preview)
			}
			if (err != nil) != tc.wantError || !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls=%v error=%v", calls, err)
			}
		})
	}
}
