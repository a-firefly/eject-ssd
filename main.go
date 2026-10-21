package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const diskutil = "/usr/sbin/diskutil"
const adbPath = "/Volumes/ssd/sdk/Android/platform-tools/adb"

type diskInfo struct {
	DeviceIdentifier, DeviceTreePath, MediaName, VirtualOrPhysical, MountPoint string
	TotalSize, Size                                                            int64
	Internal, SolidState                                                       *bool
	WholeDisk, OSInternalMedia                                                 bool
	Partitions                                                                 []diskInfo
	DiskUUID, VolumeUUID, Content                                              string
}

type containerInfo struct {
	PhysicalStores, Volumes []diskInfo
}

type diskData struct {
	AllDisksAndPartitions []diskInfo
	Containers            []containerInfo
	diskInfo
}

type disk struct{ entry, info diskInfo }
type runner func([]byte, string, ...string) ([]byte, error)
type app struct {
	run  runner
	load func(...string) (diskData, error)
	out  io.Writer
}

func run(input []byte, name string, args ...string) ([]byte, error) {
	timeout := 90 * time.Second
	if name == "/usr/sbin/lsof" {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return data, fmt.Errorf("命令超时，状态不确定；请检查磁盘状态，不要直接拔盘")
	}
	if err != nil {
		return data, fmt.Errorf("%s: %w\n%s", name, err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

func loadPlist(execute runner, args ...string) (diskData, error) {
	var data diskData
	xml, err := execute(nil, diskutil, args...)
	if err != nil {
		return data, err
	}
	// macOS ships plutil; no third-party Go package is required.
	js, err := execute(xml, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", "-")
	if err != nil {
		return data, err
	}
	if err = json.Unmarshal(js, &data); err != nil {
		return data, fmt.Errorf("无法解析磁盘信息: %w", err)
	}
	return data, nil
}

func (a *app) externalDisks() ([]disk, error) {
	data, err := a.load("list", "-plist", "external", "physical")
	if err != nil {
		return nil, err
	}
	var disks []disk
	for _, entry := range data.AllDisksAndPartitions {
		data, err := a.load("info", "-plist", entry.DeviceIdentifier)
		if err != nil {
			return nil, err
		}
		i := data.diskInfo
		if i.DeviceIdentifier == entry.DeviceIdentifier && i.Internal != nil && !*i.Internal && i.WholeDisk &&
			i.VirtualOrPhysical == "Physical" && !i.OSInternalMedia {
			disks = append(disks, disk{entry, i})
		}
	}
	return disks, nil
}

func describe(i diskInfo) string {
	kind := "介质类型未知"
	if i.SolidState != nil {
		if *i.SolidState {
			kind = "SSD"
		} else {
			kind = "HDD"
		}
	}
	size := i.TotalSize
	if size == 0 {
		size = i.Size
	}
	return fmt.Sprintf("%s  %s  %.1f GB  %s", i.DeviceIdentifier, i.MediaName, float64(size)/1e9, kind)
}

var wholeDisk = regexp.MustCompile(`^disk[0-9]+$`)

func selectDisk(disks []disk, requested string) (disk, error) {
	device := strings.TrimPrefix(requested, "/dev/")
	if requested != "" && !wholeDisk.MatchString(device) {
		return disk{}, errors.New("--disk 只能指定整盘，如 disk4 或 /dev/disk4")
	}
	var matches []disk
	for _, d := range disks {
		if requested != "" && d.info.DeviceIdentifier == device ||
			requested == "" && d.info.SolidState != nil && *d.info.SolidState {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return disk{}, errors.New("未找到符合条件的外接物理磁盘；请 --list 检查，再用 --disk 指定")
	}
	if len(matches) > 1 {
		return disk{}, errors.New("有多块外接 SSD，请 --list 查看后用 --disk 指定")
	}
	return matches[0], nil
}

func volumeDevices(entry diskInfo, containers []containerInfo) []string {
	physical := map[string]bool{entry.DeviceIdentifier: true}
	for _, p := range entry.Partitions {
		physical[p.DeviceIdentifier] = true
	}
	devices := make(map[string]bool)
	for id := range physical {
		devices[id] = true
	}
	for _, c := range containers {
		for _, store := range c.PhysicalStores {
			if physical[store.DeviceIdentifier] {
				for _, v := range c.Volumes {
					devices[v.DeviceIdentifier] = true
				}
				break
			}
		}
	}
	var result []string
	for id := range devices {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// Match the executable mapping, not just an adb process with a cwd on the disk.
func hasTargetADB(output []byte) bool {
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 9 && fields[0] == "adb" && fields[3] == "txt" &&
			strings.Join(fields[8:], " ") == adbPath {
			return true
		}
	}
	return false
}

func (a *app) diagnose(d disk) bool {
	adbOccupying := false
	data, err := a.load("apfs", "list", "-plist")
	if err != nil {
		fmt.Fprintln(a.out, "APFS 检查失败，仅检查普通分区：", err)
	}
	mounts := make(map[string]bool)
	for _, id := range volumeDevices(d.entry, data.Containers) {
		info, err := a.load("info", "-plist", id)
		if err != nil {
			fmt.Fprintln(a.out, "无法检查", id, err)
			continue
		}
		if info.MountPoint != "" {
			mounts[info.MountPoint] = true
		}
	}
	var paths []string
	for path := range mounts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		fmt.Fprintln(a.out, "未找到已挂载宗卷，请结合系统错误检查状态。")
	}
	for _, path := range paths {
		fmt.Fprintln(a.out, "\n占用检查：", path)
		out, err := a.run(nil, "/usr/sbin/lsof", "-nP", "+f", "--", path)
		if err == nil && path == "/Volumes/ssd" && hasTargetADB(out) {
			adbOccupying = true
		}
		if len(out) > 0 {
			fmt.Fprint(a.out, string(out))
		}
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			fmt.Fprintln(a.out, "占用检查失败：", err)
		} else if len(out) == 0 {
			fmt.Fprintln(a.out, "当前权限下未发现占用进程。")
		}
	}
	fmt.Fprintln(a.out, "关闭占用应用，并让其他终端 cd ~ 后重试；其他用户/系统进程可能不可见，可用 sudo 运行 --diagnose。")
	return adbOccupying
}

func (a *app) verifyDisk(d disk) error {
	disks, err := a.externalDisks()
	if err != nil {
		return err
	}
	current, err := selectDisk(disks, d.info.DeviceIdentifier)
	if err != nil {
		return err
	}
	if current.info.DeviceTreePath != d.info.DeviceTreePath || current.info.TotalSize != d.info.TotalSize ||
		current.info.MediaName != d.info.MediaName || !reflect.DeepEqual(current.entry, d.entry) {
		return errors.New("磁盘身份或分区信息已变化，请重新运行")
	}
	return nil
}

func (a *app) eject(d disk, force, preview bool) error {
	fmt.Fprintln(a.out, "目标：", describe(d.info))
	device := "/dev/" + d.info.DeviceIdentifier
	commands := [][]string{}
	if force {
		commands = append(commands, []string{"unmountDisk", "force", device})
	}
	commands = append(commands, []string{"eject", device})
	if preview {
		for _, args := range commands {
			fmt.Fprintln(a.out, "预览：", diskutil, strings.Join(args, " "))
		}
		return nil
	}
	if err := a.verifyDisk(d); err != nil {
		return err
	}
	if force {
		fmt.Fprintln(a.out, "警告：强制卸载可能丢失未保存的数据，请确保已停止复制和写入。")
	}
	adbStopped := false
	for _, args := range commands {
		out, err := a.run(nil, diskutil, args...)
		if len(out) > 0 {
			fmt.Fprint(a.out, string(out))
		}
		if err != nil {
			fmt.Fprintln(a.out, "弹出失败：", err, "\n正在检查占用……")
			occupied := a.diagnose(d)
			if !occupied || adbStopped {
				return errors.New("未能完成弹出，请检查磁盘状态后再拔线")
			}
			fmt.Fprintln(a.out, "检测到指定 adb 占用，执行：", adbPath, "kill-server")
			out, err = a.run(nil, adbPath, "kill-server")
			if len(out) > 0 {
				fmt.Fprint(a.out, string(out))
			}
			if err != nil {
				return fmt.Errorf("adb kill-server 失败，停止重试：%w", err)
			}
			adbStopped = true
			if err := a.verifyDisk(d); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "adb 已停止，重试弹出流程……")
			out, err = a.run(nil, diskutil, args...)
			if len(out) > 0 {
				fmt.Fprint(a.out, string(out))
			}
			if err != nil {
				a.diagnose(d)
				return fmt.Errorf("重试仍失败，请检查磁盘状态后再拔线：%w", err)
			}
		}
	}
	fmt.Fprintln(a.out, "已成功弹出，可以拔出 SSD。")
	return nil
}

func cli(args []string) int {
	flags := flag.NewFlagSet("eject-ssd", flag.ContinueOnError)
	device := flags.String("disk", "", "指定外接整盘，如 disk4；允许介质类型未知的磁盘")
	list := flags.Bool("list", false, "只列出外接物理磁盘")
	diagnose := flags.Bool("diagnose", false, "只检查占用，不弹出")
	preview := flags.Bool("dry-run", false, "预览目标和命令，不弹出")
	force := flags.Bool("force", false, "强制卸载后弹出，可能丢失未保存的数据")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	modes := 0
	for _, enabled := range []bool{*list, *diagnose, *preview} {
		if enabled {
			modes++
		}
	}
	if modes > 1 || (*force && (*list || *diagnose)) || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "参数错误：--list、--diagnose、--dry-run 互斥；--force 只能用于弹出或预览；不接受位置参数。")
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "仅支持 macOS。")
		return 1
	}
	if err := os.Chdir("/"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	a := &app{run: run, out: os.Stdout}
	a.load = func(args ...string) (diskData, error) { return loadPlist(a.run, args...) }
	disks, err := a.externalDisks()
	if err == nil && *list {
		if len(disks) == 0 {
			fmt.Fprintln(a.out, "未找到外接物理磁盘。")
		}
		for _, d := range disks {
			fmt.Fprintln(a.out, describe(d.info))
		}
		return 0
	}
	if err == nil {
		var d disk
		d, err = selectDisk(disks, *device)
		if err == nil {
			if *diagnose {
				fmt.Fprintln(a.out, "目标：", describe(d.info))
				a.diagnose(d)
			} else {
				err = a.eject(d, *force, *preview)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	return 0
}

func main() { os.Exit(cli(os.Args[1:])) }
