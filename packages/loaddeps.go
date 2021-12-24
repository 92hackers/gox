/*
 Copyright 2021 The GoPlus Authors (goplus.org)
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at
     http://www.apache.org/licenses/LICENSE-2.0
 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package packages

/*
import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ----------------------------------------------------------------------------

type pkgRef struct {
	Export string
}

func loadDeps(modRoot string, pkgPaths ...string) (pkgs map[string]*pkgRef, err error) {
	dir := modRoot + "/.gop"
	file := dir + "/dummy.go"
	os.MkdirAll(dir, 0777)

	var buf bytes.Buffer
	buf.WriteString(`package main

import (
`)
	for _, pkgPath := range pkgPaths {
		fmt.Fprintf(&buf, "\t_ \"%s\"\n", pkgPath)
	}
	buf.WriteString(`)

func main() {
}
`)
	err = os.WriteFile(file, buf.Bytes(), 0666)
	if err != nil {
		return
	}
	defer os.Remove(file)
	return loadDepPkgs("", file)
}

func loadDepPkgs(dir, src string) (pkgs map[string]*pkgRef, err error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("go", "run", "-n", "-x", src)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = dir
	err = cmd.Run()
	if err != nil {
		return nil, &ExecCmdError{Err: err}
	}
	pkgs = make(map[string]*pkgRef)
	err = loadDepPkgsFrom(pkgs, stderr.String())
	return
}

func loadDepPkgsFrom(pkgs map[string]*pkgRef, data string) (err error) {
	const packagefile = "packagefile "
	const vendor = "vendor/"
	for data != "" {
		pos := strings.IndexByte(data, '\n')
		if strings.HasPrefix(data, packagefile) {
			line := data[len(packagefile):pos]
			if t := strings.Index(line, "="); t > 0 {
				if pkgPath := line[:t]; pkgPath != "command-line-arguments" {
					pkgPath = strings.TrimPrefix(pkgPath, vendor)
					pkgs[pkgPath] = &pkgRef{Export: line[t+1:]}
				}
			}
		}
		if pos < 0 {
			break
		}
		data = data[pos+1:]
	}
	return
}
*/
// ----------------------------------------------------------------------------
