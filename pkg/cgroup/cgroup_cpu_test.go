// Copyright 2023 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package cgroup

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/pingcap/log"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func checkKernelVersionNewerThan(t *testing.T, major, minor int) bool {
	u := syscall.Utsname{}
	err := syscall.Uname(&u)
	require.NoError(t, err)
	releaseBs := make([]byte, 0, len(u.Release))
	for _, v := range u.Release {
		if v == 0 {
			break
		}
		releaseBs = append(releaseBs, byte(v))
	}
	releaseStr := string(releaseBs)
	log.Info("kernel release string", zap.String("release-str", releaseStr))
	versionInfoRE := regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)
	kernelVerion := versionInfoRE.FindAllString(releaseStr, 1)
	require.Equal(t, 1, len(kernelVerion), fmt.Sprintf("release str is %s", releaseStr))
	kernelVersionPartRE := regexp.MustCompile(`[0-9]+`)
	kernelVersionParts := kernelVersionPartRE.FindAllString(kernelVerion[0], -1)
	require.Equal(t, 3, len(kernelVersionParts), fmt.Sprintf("kernel verion str is %s", kernelVerion[0]))
	log.Info("parsed kernel version parts", zap.String("major", kernelVersionParts[0]), zap.String("minor", kernelVersionParts[1]), zap.String("patch", kernelVersionParts[2]))
	mustConvInt := func(s string) int {
		i, err := strconv.Atoi(s)
		require.NoError(t, err, s)
		return i
	}
	versionNewerThanFlag := false
	if mustConvInt(kernelVersionParts[0]) > major {
		versionNewerThanFlag = true
	} else {
		if mustConvInt(kernelVersionParts[0]) == major && mustConvInt(kernelVersionParts[1]) > minor {
			versionNewerThanFlag = true
		}
	}
	return versionNewerThanFlag
}

func TestGetCgroupCPU(t *testing.T) {
	exit := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-exit:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}
	cpu, err := GetCgroupCPU()
	if err == errNoCPUControllerDetected {
		// for more information, please refer https://github.com/pingcap/tidb/pull/41347
		if checkKernelVersionNewerThan(t, 4, 7) {
			require.NoError(t, err, "linux version > v4.7 and err still happens")
		} else {
			log.Info("the 'no cpu controller detected' error is ignored because the kernel is too old")
		}
	} else {
		require.NoError(t, err)
		require.NotZero(t, cpu.Period)
		require.Less(t, int64(1), cpu.Period)
	}
	close(exit)
	wg.Wait()
}
