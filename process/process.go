package process

import (
	"fmt"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/op/go-logging"
	"github.com/patrickmn/go-cache"
	"github.com/shirou/gopsutil/process"
	"github.com/zero-os/0-ork/utils"
)

var log = logging.MustGetLogger("ORK")

// whiteListNames is slice of processes names that should never be killed.
var whitelistNames = map[string]struct{}{
	"0-ork":              struct{}{},
	"qemu-system-x86_64": struct{}{},
	"libvirtd":           struct{}{},
	"coreX":              struct{}{},
	"core0":              struct{}{},
	"kthreadd":           struct{}{},
	"g8ufs":              struct{}{},
}
var killableKidsNames = map[string]struct{}{
	"core0": struct{}{},
	"coreX": struct{}{},
}

type processesMap map[int32]*process.Process
type whiteListMap map[int32]struct{}
type killableKidsPids map[int32]struct{}

// Processes is a struct of a list of process.Process and a function to be
// used to sort the list.
type Process struct {
	process  *process.Process
	memUsage uint64
	cpuTime  ewma.MovingAverage
	cpuDelta func(uint64) uint64
	name     string
}

func (p *Process) CPU() float64 {
	return p.cpuTime.Value()
}

func (p *Process) Memory() uint64 {
	return p.memUsage
}

func (p *Process) Priority() int {
	return 10
}

func (p *Process) Name() string {
	return p.name
}

func (p *Process) Kill() error {
	proc := p.process
	pid := proc.Pid

	name, err := proc.Name()
	if err != nil {
		log.Error("Error getting process name")
		name = "unknown"
	}

	utils.LogToKernel("ORK: attempting to kill process with pid %v and name %v\n", pid, name)

	if err = proc.Kill(); err != nil {
		utils.LogToKernel("ORK: error killing process with pid %v and name %v\n", pid, name)
		log.Errorf("Error killing process %v %v", pid, name)
		return err
	}

	utils.LogToKernel("ORK: successfully killed process with pid %v and name %v\n", pid, name)
	log.Infof("Successfully killed process %v %v", pid, name)
	return nil
}
func UpdateCache(c *cache.Cache) {
	pMap, err := makeProcessesMap()
	if err != nil {
		log.Errorf("Error getting processes: %v", err)
	}

	whiteList, killableKids := setupWhiteList(pMap)

	for pid, proc := range pMap {
		if killable, err := isProcessKillable(proc, pMap, whiteList, killableKids); err != nil {
			log.Errorf("Error checking if process is killable: %v", err)
			continue
		} else if killable == false {
			continue
		}

		times, err := proc.Times()
		if err != nil {
			log.Errorf("Error getting process cpu percentage: %v", err)
			continue
		}
		total := times.Total()
		nanoSeconds := time.Duration(total) * time.Second / time.Nanosecond

		memory, err := proc.MemoryInfo()
		if err != nil {
			log.Errorf("Error getting process memory info: %v", err)
			continue
		}
		var cachedProcess *Process
		key := fmt.Sprint(pid)
		p, ok := c.Get(key)
		if ok {
			cachedProcess = p.(*Process)
			cachedProcess.cpuTime.Add(float64(cachedProcess.cpuDelta(uint64(nanoSeconds))))
		} else {
			cachedProcess = &Process{
				name:     key,
				process:  proc,
				cpuDelta: utils.Delta(uint64(nanoSeconds)),
				cpuTime:  ewma.NewMovingAverage(60),
			}
		}
		cachedProcess.memUsage = memory.RSS / (1024. * 1024.) //convert byte to mega byte
		c.Set(key, cachedProcess, time.Minute)
	}
}

// MakeProcessesMap returns a map of process pid and process.Process instance for all running processes
func makeProcessesMap() (processesMap, error) {
	pMap := make(processesMap)

	processesIds, err := process.Pids()
	if err != nil {
		return nil, err
	}

	for _, pid := range processesIds {
		p, err := process.NewProcess(pid)
		if err != nil {
			return nil, err
		}
		pMap[p.Pid] = p
	}

	return pMap, nil
}

// SetupWhiteList returns a map of pid and process.Process instance for whitelisted processes.
func setupWhiteList(pMap processesMap) (whiteListMap, killableKidsPids) {
	whiteList := make(whiteListMap)
	killableKids := make(killableKidsPids)
	for _, p := range pMap {
		processName, err := p.Name()
		if err != nil {
			log.Errorf("Erorr getting process name for %v", p.Pid)
			continue
		}

		_, ok := whitelistNames[processName]
		if !ok {
			continue
		}
		whiteList[p.Pid] = struct{}{}
		_, ok = killableKidsNames[processName]
		if !ok {
			continue
		}
		killableKids[p.Pid] = struct{}{}
	}

	return whiteList, killableKids
}

// IsProcessKillable checks if a process can be killed or not.
// A process can't be killed if it is a member of the whiteList or if it is a child of a process in the
// whiteList.
func isProcessKillable(p *process.Process, pMap processesMap, whiteList whiteListMap, killableKids killableKidsPids) (bool, error) {
	_, ok := whiteList[p.Pid]
	if ok {
		return false, nil
	}
	return isParentKillable(p, pMap, whiteList, killableKids)
}

func isParentKillable(p *process.Process, pMap processesMap, whiteList whiteListMap, killableKids killableKidsPids) (bool, error) {
	pPid, err := p.Ppid()
	if err != nil {
		log.Errorf("Error getting parent pid for pid %v", p.Pid)
		return false, err
	}

	_, ok := whiteList[pPid]
	if ok {
		_, ok = killableKids[pPid]
		if ok {
			return true, nil
		}
		return false, nil
	}

	parent, inMap := pMap[pPid]
	if inMap != true {
		message := fmt.Sprintf("Error getting parent process %v of process %v from process map", pPid, p.Pid)
		log.Error(message)
		return false, fmt.Errorf(message)
	}
	return isParentKillable(parent, pMap, whiteList, killableKids)
}
