package main

import (
	"fmt"
	"io/ioutil"
	"strings"

	"path"

	"os"
	"sync"

	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type CgroupsInfo struct {
	SubsysName string
	Hierarchy  uint32
	NumCgroups uint32
	Enabled    bool
}

func getCgroups() (cgroups map[string]CgroupsInfo, err error) {
	var out []byte
	var n int

	out, err = ioutil.ReadFile("/proc/cgroups")
	if err != nil {
		return nil, err
	}
	cgroups = make(map[string]CgroupsInfo)

	for i, line := range strings.Split(string(out), "\n") {
		var subinfo CgroupsInfo
		var enabled int
		if i == 0 || line == "" {
			continue
		}
		n, err = fmt.Sscanf(line, "%s %d %d %d", &subinfo.SubsysName, &subinfo.Hierarchy, &subinfo.NumCgroups, &enabled)
		if n != 4 || err != nil {
			if err == nil {
				err = fmt.Errorf("failed to parse /proc/cgroup entry %s", line)
			}
			return
		}
		subinfo.Enabled = enabled == 1
		cgroups[subinfo.SubsysName] = subinfo
	}
	return
}

// this info read from the /proc/[pid]/mountinfo
// The file contains lines of the form:
//
// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
// (1)(2)(3)   (4)   (5)      (6)      (7)   (8) (9)   (10)         (11)
//
// Please see more on http://man7.org/linux/man-pages/man5/proc.5.html
type MountInfo struct {
	// (1) mount ID: a unique ID for the mount(may be reused after umount(2)).
	MountId uint32
	// (2) parent ID: the ID of the parent mount (or of self for the top of the mount tree).
	ParentId uint32
	// (3) major: the value of st_dev for files on this filesystem (see stat(2)).
	DevMajor uint32
	// (3) minor: the value of st_dev for files on this filesystem (see stat(2)).
	DevMinor uint32
	// (4) root: the pathname of the directory in the filesystem which forms the root of this mount.
	Root string
	// (5) mount point: the pathname of the mount point relative to the process's root directory.
	MountPoint string
	// (6) mount options: per-mount options.
	MountOption string
	// (8) optional fields: zero or more fields of the form "tag[:value]"; see below.
	OptionField string
	// (9) filesystem type: the filesystem type in the form "type[.subtype]".
	FsType string
	// (10) mount source: filesystem-specific information or "none".
	MountSource string
	// (11) super options: per-superblock options.
	SuperOption string
}

func getMountInfo() (mount []MountInfo, err error) {
	var out []byte
	var n int
	out, err = ioutil.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	mount = make([]MountInfo, 0)

	for i, line := range strings.Split(string(out), "\n") {
		var subinfo MountInfo

		if i == 0 || line == "" {
			continue
		}
		sepindex := strings.Index(line, "-")
		// parse the 1 - 6 field
		n, err = fmt.Sscanf(line, "%d %d %d:%d %s %s %s", &subinfo.MountId, &subinfo.ParentId, &subinfo.DevMajor, &subinfo.DevMinor, &subinfo.Root, &subinfo.MountPoint, &subinfo.MountOption)

		if n != 7 || err != nil {
			if err == nil {
				err = fmt.Errorf("failed to parse /proc/self/mountinfo entry %s", line)
			}
			return
		}
		// parse the field after sep '-'
		n, err = fmt.Sscanf(line[sepindex+1:], "%s %s %s", &subinfo.FsType, &subinfo.MountSource, &subinfo.SuperOption)
		if n != 3 || err != nil {
			if err == nil {
				err = fmt.Errorf("failed to parse /proc/self/mountinfo entry %s", line)
			}
			return
		}

		// process some item like:
		// 33 29 0:27 / /sys/fs/cgroup/net_cls,net_prio rw,nosuid,nodev,noexec,relatime shared:14 - cgroup cgroup rw,net_cls,net_prio
		if strings.Contains(subinfo.MountPoint, ",") {
			dirPath := path.Dir(subinfo.MountPoint)
			for _, v := range strings.Split(path.Base(subinfo.MountPoint), ",") {

				var sub MountInfo
				sub = subinfo
				sub.MountPoint = path.Join(dirPath, v)
				mount = append(mount, sub)
			}
		} else {

			mount = append(mount, subinfo)
		}
	}
	return

}

type Container struct {
	id         string
	cgroupPath map[string]string
	current    *cgroups.Stats
	previous   *cgroups.Stats
	mutex      sync.Mutex
}

func NewContainer(id string) (container *Container, err error) {
	var docker Container
	docker.cgroupPath = make(map[string]string)
	cpath := make(map[string]string)
	cpath, err = getCgroupsPath()
	if err != nil {
		return
	}
	for k := range cpath {
		docker.cgroupPath[k] = path.Join(cpath[k], "docker", id)
	}
	container = &docker
	return
}

func getCgroupsPath() (cpath map[string]string, err error) {
	var cgroupDict map[string]CgroupsInfo
	var mountList []MountInfo

	cpath = make(map[string]string)

	cgroupDict, err = getCgroups()
	mountList, err = getMountInfo()
	for _, mnt := range mountList {
		if mnt.FsType != "cgroup" {
			continue
		}
		baseName := path.Base(mnt.MountPoint)
		if cgroupDict[baseName].Enabled {
			cpath[baseName] = mnt.MountPoint
		}
	}
	return
}

func getCurrentStat() (err error) {
	containerList, err := GetContainerList()
	if err != nil {
		return
	}
	if err != nil {
		return
	}
	for _, container := range containerList {
		my, err := NewContainer(container)
		if err != nil {
			log.Warnf("get stat error id:%s, error:%s", container, err.Error())
		}
		my.Update()
	}

	return
}

func (this *Container) Update() {
	manager := &fs.Manager{
		Cgroups: &configs.Cgroup{
			Name: this.id,
		},
		Paths: this.cgroupPath,
	}

	this.mutex.Lock()
	defer this.mutex.Unlock()
	stat, err := manager.GetStats()
	if err != nil {
		fmt.Println(err.Error())
	}
	this.current = stat
	this.UpdateCpu(stat.CpuStats)
	this.previous = stat
	fmt.Println(this.current.CpuStats.CpuUsage.PercpuUsage) //	fmt.Println(stat.CpuStats)
	//	fmt.Println(stat.PidsStats)
	//	fmt.Println(stat.MemoryStats)
	//	fmt.Println(stat.BlkioStats)
}

func (this *Container) UpdateCpu(stat cgroups.CpuStats) {

	// first run the previous is nil
	if this.previous == nil {
		return
	}
	this.current.CpuStats.CpuUsage.TotalUsage = stat.CpuUsage.TotalUsage - this.previous.CpuStats.CpuUsage.TotalUsage
	n := len(stat.CpuUsage.PercpuUsage)

	for i := 0; i < n; i++ {
		this.current.CpuStats.CpuUsage.PercpuUsage[i] = stat.CpuUsage.PercpuUsage[i] - this.previous.CpuStats.CpuUsage.PercpuUsage[i]
	}
	this.current.CpuStats.CpuUsage.UsageInKernelmode = stat.CpuUsage.UsageInKernelmode - this.previous.CpuStats.CpuUsage.UsageInKernelmode
	this.current.CpuStats.CpuUsage.UsageInUsermode = stat.CpuUsage.UsageInUsermode - this.previous.CpuStats.CpuUsage.UsageInUsermode

}

// get the list of the container from cgroup/subsystem/docker
// like /sys/fs/cgroup/cpu/docker
func GetContainerList() (containerList []string, err error) {
	var cpath map[string]string
	var flist []os.FileInfo
	cpath, err = getCgroupsPath()
	if err != nil {
		return
	}
	for _, sub := range cpath {
		dockerDir := path.Join(sub, "docker")
		flist, err = ioutil.ReadDir(dockerDir)
		if err != nil {
			return
		}
		for _, f := range flist {
			if f.IsDir() && len(f.Name()) == 64 {
				containerList = append(containerList, f.Name())
			}
		}
		if len(containerList) != 0 {
			return
		}
	}
	return
}

func main() {
	log.Info("start")
	for {
		getCurrentStat()
		time.Sleep(3 * time.Second)
	}
	//fmt.Println(getCgroups())
	//fmt.Println(getMountInfo())
	//fmt.Println(getCgroupsPath())
	//fmt.Println(GetContainerList())
}
