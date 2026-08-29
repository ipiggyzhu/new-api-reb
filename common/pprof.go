package common

import (
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/shirou/gopsutil/cpu"
)

// Monitor 定时监控cpu使用率，超过阈值输出pprof文件
func Monitor() {
	for {
		percent, err := cpu.Percent(time.Second, false)
		if err != nil || len(percent) == 0 {
			// This runs in a bare goroutine, so a panic here would kill the
			// whole gateway. A transient /proc read failure is not fatal.
			if err != nil {
				SysError("读取CPU使用率失败: " + err.Error())
			}
			time.Sleep(30 * time.Second)
			continue
		}
		if percent[0] > 80 {
			fmt.Println("cpu usage too high")
			// write pprof file
			if _, err := os.Stat("./pprof"); os.IsNotExist(err) {
				err := os.Mkdir("./pprof", os.ModePerm)
				if err != nil {
					SysLog("创建pprof文件夹失败 " + err.Error())
					continue
				}
			}
			f, err := os.Create("./pprof/" + fmt.Sprintf("cpu-%s.pprof", time.Now().Format("20060102150405")))
			if err != nil {
				SysLog("创建pprof文件失败 " + err.Error())
				continue
			}
			err = pprof.StartCPUProfile(f)
			if err != nil {
				SysLog("启动pprof失败 " + err.Error())
				continue
			}
			time.Sleep(10 * time.Second) // profile for 30 seconds
			pprof.StopCPUProfile()
			f.Close()
		}
		time.Sleep(30 * time.Second)
	}
}
