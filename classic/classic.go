// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2015 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package classic

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ubuntu-core/snappy/arch"
	"github.com/ubuntu-core/snappy/dirs"
	"github.com/ubuntu-core/snappy/helpers"
	"github.com/ubuntu-core/snappy/progress"
	"github.com/ubuntu-core/snappy/release"
)

const (
	lxdBaseURL  = "https://images.linuxcontainers.org"
	lxdIndexURL = lxdBaseURL + "/meta/1.0/index-system"
)

// Enabled returns true if the classic mode is already enabled
func Enabled() bool {
	return helpers.FileExists(filepath.Join(dirs.ClassicDir, "etc", "apt", "sources.list"))
}

func mountpoint(path string) bool {
	err := exec.Command("mountpoint", path).Run()
	// man-page: zero if the directory is a mountpoint, non-zero if not
	return err == nil
}

func bindmount(src, dstPath, remountArg string) error {
	dst := filepath.Join(dirs.ClassicDir, dstPath)
	// already mounted
	if mountpoint(dst) {
		return nil
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() && (st.Mode()&os.ModeSymlink == 0) {
		if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
			return err
		}
	}
	cmd := exec.Command("mount", "--make-rprivate", "--rbind", "-o", "rbind", src, dst)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bind mounting %s to %s failed: %s (%s)", src, dst, err, output)
	}

	if remountArg != "" {
		cmd := exec.Command("mount", "--rbind", "-o", "remount,"+remountArg, src, dst)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("remount %s to %s failed: %s (%s)", src, dst, err, output)
		}
	}

	return nil
}

var bindMountDirs = [][3]string{
	{"/home", "/home", ""},
	{"/run", "/run", ""},
	{"/proc", "/proc", ""},
	{"/sys", "/sys", ""},
	{"/var/lib/extrausers", "/var/lib/extrausers", "ro"},
	{"/etc/sudoers", "/etc/sudoers", "ro"},
	{"/etc/sudoers.d", "/etc/sudoers.d", "ro"},
	{"/", "/snappy", ""},
}

// Run runs a shell in the classic environment
func Run() error {
	for _, l := range bindMountDirs {
		src := l[0]
		dst := l[1]
		args := l[2]
		if err := bindmount(src, dst, args); err != nil {
			return err
		}
	}

	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		sudoUser = "root"
	}

	cmd := exec.Command("systemd-run", "--quiet", "--scope", "--unit=snappy-classic.scope", "--description=Snappy Classic shell", "chroot", dirs.ClassicDir, "sudo", "debian_chroot=classic", "-u", sudoUser, "-i")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	// kill leftover processes after exiting, if it's still around
	cmd = exec.Command("systemctl", "stop", "snappy-classic.scope")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to cleanup classic: %s (%s)", err, output)
	}

	return nil
}

func findDownloadPath(r io.Reader) (string, error) {
	arch := arch.UbuntuArchitecture()
	lsb, err := release.ReadLsb()
	if err != nil {
		return "", err
	}
	release := lsb.Codename

	needle := fmt.Sprintf("ubuntu;%s;%s;default", release, arch)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), needle) {
			l := strings.Split(scanner.Text(), ";")
			if len(l) < 6 {
				return "", fmt.Errorf("can not find download path in %s", scanner.Text())
			}
			return l[5], nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("needle %s not found", needle)
}

func findDownloadUrl() (string, error) {
	resp, err := http.Get(lxdIndexURL)
	if err != nil {
		return "", fmt.Errorf("failed to downlaod lxdIndexUrl: %s", err)
	}
	defer resp.Body.Close()

	dlPath, err := findDownloadPath(resp.Body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s%s%s", lxdBaseURL, dlPath, "rootfs.tar.xz")

	return url, nil
}

func downloadFile(url string, pbar progress.Meter) (string, error) {
	name := "classic"

	w, err := ioutil.TempFile("", name)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(w.Name())
		}
	}()
	defer w.Close()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download %s: %s", url, resp.StatusCode)
	}

	if pbar != nil {
		pbar.Start(name, float64(resp.ContentLength))
		mw := io.MultiWriter(w, pbar)
		_, err = io.Copy(mw, resp.Body)
		pbar.Finished()
	} else {
		_, err = io.Copy(w, resp.Body)
	}

	return w.Name(), err
}

var policyRc = []byte(`
#!/bin/sh
while true; do
    case "\$1" in
      -*) shift ;;
      makedev) exit 0;;
      x11-common) exit 0;;
      *) exit 101;;
    esac
done
`)

// Create creates a new classic shell envirionment
func Create() error {
	targetDir := dirs.ClassicDir

	if Enabled() {
		return fmt.Errorf("clasic mode already created in %s", targetDir)
	}
	url, err := findDownloadUrl()
	if err != nil {
		return err
	}

	pbar := progress.NewTextProgress()
	fname, err := downloadFile(url, pbar)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create classic mode dir: %s", err)
	}

	cmd := exec.Command("tar", "-C", targetDir, "-xpf", fname)
	if cmd.Run() != nil {
		return fmt.Errorf("failed to unpack %s: %s", fname, err)
	}

	// copy configs
	for _, f := range []string{"hostname", "hosts", "timezone", "localtime"} {
		src := filepath.Join("/etc/", f)
		dst := filepath.Join(targetDir, "etc", f)
		if err := helpers.CopyFile(src, dst, helpers.CopyFlagPreserveAll); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile(filepath.Join(targetDir, "usr/sbin/policy-rc.d"), policyRc, 0755); err != nil {
		return fmt.Errorf("failed to write policy-rc.d: %s", err)
	}

	// remove ubuntu user, will come from snappy OS
	if output, err := exec.Command("chroot", targetDir, "deluser", "ubuntu").CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove ubuntu user from chroot: %s (%s)", err, output)
	}

	// install extra packages; make sure chroot can resolve DNS
	resolveDir := filepath.Join(targetDir, "/run/resolvconf/")
	if err := os.MkdirAll(resolveDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %s", resolveDir, err)
	}
	src := "/run/resolvconf/resolv.conf"
	dst := filepath.Join(targetDir, "/run/resolvconf/")
	if err := helpers.CopyFile(src, dst, helpers.CopyFlagPreserveAll); err != nil {
		return fmt.Errorf("failed to copy %s to %s", src, dst)
	}

	// enable libnss-extrausers
	cmd = exec.Command("chroot", targetDir, "apt", "install", "-y", "libnss-extrausers")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd = exec.Command("sed", "-i", "-r", "/^(passwd|group|shadow):/ s/$/ extrausers/", filepath.Join(targetDir, "/etc/nsswitch.conf"))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to enable libness-extrausers: %s", output)
	}

	// clean up cruft (bad lxd rootfs!)
	if output, err := exec.Command("sh", "-c", fmt.Sprintf("rm -rf %s/run/*", targetDir)).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to cleanup classic /run dir: %s (%s)", err, output)
	}

	return nil
}

// Destroy destroys a classic environment
func Destroy() error {
	return fmt.Errorf("no implemented yet, need to undo bind mounts")
	/*
		cmd := exec.Command("rm", "-rf", dirs.ClassicDir)
		return cmd.Run()
	*/
}
