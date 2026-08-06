// Package libvirt manages local or remote libvirt domains through virsh.
package libvirt

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

var runner shell.Runner = shell.Real{}

func SetRunner(r shell.Runner) { runner = r }

type Client struct{ URI string }

func NewClient(uri string) Client {
	if uri == "" {
		uri = config.LibvirtURI()
	}
	return Client{URI: uri}
}
func (c Client) run(args ...string) ([]byte, error) {
	return runner.Run("virsh", append([]string{"-c", c.URI}, args...)...)
}

// DumpXML returns a domain's definition, which is where libvirt records the
// facts no listing carries — firmware above all.
func (c Client) DumpXML(name string) ([]byte, error) { return c.run("dumpxml", name) }

func (c Client) List() ([]types.VM, error) {
	out, err := c.run("list", "--all", "--name")
	if err != nil {
		return nil, fmt.Errorf("libvirt list: %w", err)
	}
	var vms []types.VM
	for _, name := range strings.Fields(string(out)) {
		state, _ := c.run("domstate", name)
		info, _ := c.run("dominfo", name)
		running := strings.Contains(strings.ToLower(string(state)), "running")
		vm := types.VM{Name: name, Backend: "libvirt", Namespace: "libvirt", Context: c.URI, Node: c.URI, Status: strings.TrimSpace(string(state)), Running: running, Ready: running}
		for _, line := range strings.Split(string(info), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "CPU(s)":
				vm.CPU, _ = strconv.Atoi(v)
			case "Max memory":
				vm.Mem = v
			}
		}
		vm.SetIdentity()
		vms = append(vms, vm)
	}
	return vms, nil
}
func (c Client) Exists(name string) bool { _, err := c.run("dominfo", name); return err == nil }
func (c Client) Start(name string) error { _, err := c.run("start", name); return err }

// Restart is `virsh reboot`: an ACPI reboot request to the guest, which is
// libvirt's own mechanism and preserves the guest's shutdown ordering. It needs
// the guest to be listening for ACPI — a guest that ignores it stays up, which
// is the honest outcome and not something to paper over with a destroy.
func (c Client) Restart(name string) error { _, err := c.run("reboot", name); return err }

// Pause and Resume are virsh suspend/resume: the domain's vCPUs stop, its
// memory stays allocated, and resume continues from that exact point. Distinct
// from destroy/start, which is a reboot.
func (c Client) Pause(name string) error  { _, err := c.run("suspend", name); return err }
func (c Client) Resume(name string) error { _, err := c.run("resume", name); return err }
func (c Client) Stop(name string) error   { _, err := c.run("shutdown", name); return err }
func (c Client) Delete(name string) error {
	_, _ = c.run("destroy", name)
	_, err := c.run("undefine", name, "--remove-all-storage", "--nvram")
	return err
}
func (c Client) Info(name string) ([]byte, error) { return c.run("dominfo", name) }
func (c Client) SSH(name, command string) error {
	return fmt.Errorf("libvirt domain %q has no discovered SSH address; configure guest networking and use ssh directly", name)
}
func (c Client) Viewer(name string) error {
	cmd := exec.Command("virt-viewer", "--connect", c.URI, name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// DialVNC opens the domain's advertised VNC display. For qemu+ssh libvirt
// URIs, loopback display addresses live on the hypervisor, so the connection
// is carried through ssh -W instead of assuming this host can reach it.
func (c Client) DialVNC(name string) (io.ReadWriteCloser, error) {
	out, err := c.run("domdisplay", name, "--type", "vnc")
	if err != nil {
		return nil, fmt.Errorf("libvirt display: %w", err)
	}
	display, err := url.Parse(strings.TrimSpace(string(out)))
	if err != nil || display.Scheme != "vnc" || display.Host == "" {
		return nil, fmt.Errorf("domain %q does not advertise a VNC display", name)
	}
	host, port := display.Hostname(), display.Port()
	if port == "" {
		port = "5900"
	}
	addr := net.JoinHostPort(host, port)
	libvirtURI, _ := url.Parse(c.URI)
	if strings.Contains(libvirtURI.Scheme, "+ssh") && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return dialSSHStream(libvirtURI.User, libvirtURI.Hostname(), addr)
	}
	return net.Dial("tcp", addr)
}

type commandStream struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (s *commandStream) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *commandStream) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *commandStream) Close() error {
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.cmd.Wait()
}
func dialSSHStream(user *url.Userinfo, host, destination string) (io.ReadWriteCloser, error) {
	if host == "" {
		return nil, fmt.Errorf("libvirt SSH URI has no host")
	}
	target := host
	if user != nil && user.Username() != "" {
		target = user.Username() + "@" + host
	}
	cmd := exec.Command("ssh", "-T", "-W", destination, target)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	return &commandStream{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}
func (c Client) Create(opts types.CreateOpts) error {
	cpu := opts.CPU
	if cpu < 1 {
		cpu = 2
	}
	args := []string{"--connect", c.URI, "--name", opts.Name, "--vcpus", strconv.Itoa(cpu), "--memory", memoryMiB(opts.Mem), "--noautoconsole"}
	if opts.QCOW != "" {
		args = append(args, "--import", "--disk", "path="+opts.QCOW)
	} else if opts.ISO != "" {
		disk := strings.TrimSuffix(opts.Disk, "G")
		if disk == "" {
			disk = "20"
		}
		args = append(args, "--cdrom", opts.ISO, "--disk", "size="+disk)
	} else {
		return fmt.Errorf("libvirt create needs --iso or --qcow")
	}
	cmd := exec.Command("virt-install", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virt-install: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
func memoryMiB(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "4096"
	}
	if strings.HasSuffix(s, "G") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "G"))
		return strconv.Itoa(n * 1024)
	}
	return strings.TrimSuffix(s, "M")
}

// Metrics reports a domain's live CPU and memory usage from virsh domstats.
//
// cpu.time is cumulative nanoseconds, so CPU is sampled twice a known interval
// apart and reported as a rate — a single reading would be lifetime CPU time
// presented as though it were current usage.
//
// Memory is balloon.rss, the host-side resident size of the QEMU process,
// which is what the host is actually spending. balloon.current is what the
// guest was *given* and stays flat at its configured size, so it looks like a
// working metric while telling you nothing.
func (c Client) Metrics(name string) (map[string]string, error) {
	res := map[string]string{"cpu": "", "mem": ""}

	first, err := c.domStats(name)
	if err != nil {
		return res, err
	}
	if rss, ok := first["balloon.rss"]; ok && rss > 0 {
		res["mem"] = humanBytes(rss * 1024) // domstats reports KiB
	}
	cpuFirst, ok := first["cpu.time"]
	if !ok {
		return res, nil
	}

	time.Sleep(cpuSampleInterval)
	second, err := c.domStats(name)
	if err != nil {
		return res, nil // memory already read; a failed second sample is not fatal
	}
	cpuSecond, ok := second["cpu.time"]
	if !ok || cpuSecond < cpuFirst {
		return res, nil
	}
	percent := float64(cpuSecond-cpuFirst) / float64(cpuSampleInterval.Nanoseconds()) * 100
	res["cpu"] = fmt.Sprintf("%.0f%%", percent)
	return res, nil
}

// cpuSampleInterval is the gap between the two cpu.time readings. Long enough
// to be stable against scheduler jitter, short enough that a polling fleet view
// does not feel it.
var cpuSampleInterval = 200 * time.Millisecond

// domStats parses `virsh domstats --raw` output: "  name=value" lines under a
// "Domain: 'x'" header. Only the numeric fields are kept, which is all the
// caller wants.
func (c Client) domStats(name string) (map[string]int64, error) {
	out, err := c.run("domstats", name, "--cpu-total", "--balloon", "--raw")
	if err != nil {
		return nil, fmt.Errorf("virsh domstats %s: %w", name, err)
	}
	stats := map[string]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			stats[strings.TrimSpace(key)] = n
		}
	}
	return stats, nil
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%dKi", b/1024)
}
