// +build linux

package libpod

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/podman/v3/pkg/errorhandling"
	"github.com/containers/podman/v3/pkg/rootlessport"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type slirpFeatures struct {
	HasDisableHostLoopback bool
	HasMTU                 bool
	HasEnableSandbox       bool
	HasEnableSeccomp       bool
	HasCIDR                bool
	HasOutboundAddr        bool
	HasIPv6                bool
}

type slirp4netnsCmdArg struct {
	Proto     string `json:"proto,omitempty"`
	HostAddr  string `json:"host_addr"`
	HostPort  int32  `json:"host_port"`
	GuestAddr string `json:"guest_addr"`
	GuestPort int32  `json:"guest_port"`
}

type slirp4netnsCmd struct {
	Execute string            `json:"execute"`
	Args    slirp4netnsCmdArg `json:"arguments"`
}

type slirp4netnsNetworkOptions struct {
	cidr                string
	disableHostLoopback bool
	enableIPv6          bool
	isSlirpHostForward  bool
	noPivotRoot         bool
	mtu                 int
	outboundAddr        string
	outboundAddr6       string
}

func checkSlirpFlags(path string) (*slirpFeatures, error) {
	cmd := exec.Command(path, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "slirp4netns %q", out)
	}
	return &slirpFeatures{
		HasDisableHostLoopback: strings.Contains(string(out), "--disable-host-loopback"),
		HasMTU:                 strings.Contains(string(out), "--mtu"),
		HasEnableSandbox:       strings.Contains(string(out), "--enable-sandbox"),
		HasEnableSeccomp:       strings.Contains(string(out), "--enable-seccomp"),
		HasCIDR:                strings.Contains(string(out), "--cidr"),
		HasOutboundAddr:        strings.Contains(string(out), "--outbound-addr"),
		HasIPv6:                strings.Contains(string(out), "--enable-ipv6"),
	}, nil
}

func parseSlirp4netnsNetworkOptions(r *Runtime, extraOptions []string) (*slirp4netnsNetworkOptions, error) {
	slirpOptions := append(r.config.Engine.NetworkCmdOptions, extraOptions...)
	slirp4netnsOpts := &slirp4netnsNetworkOptions{
		// overwrite defaults
		disableHostLoopback: true,
		mtu:                 slirp4netnsMTU,
		noPivotRoot:         r.config.Engine.NoPivotRoot,
	}
	for _, o := range slirpOptions {
		parts := strings.SplitN(o, "=", 2)
		if len(parts) < 2 {
			return nil, errors.Errorf("unknown option for slirp4netns: %q", o)
		}
		option, value := parts[0], parts[1]
		switch option {
		case "cidr":
			ipv4, _, err := net.ParseCIDR(value)
			if err != nil || ipv4.To4() == nil {
				return nil, errors.Errorf("invalid cidr %q", value)
			}
			slirp4netnsOpts.cidr = value
		case "port_handler":
			switch value {
			case "slirp4netns":
				slirp4netnsOpts.isSlirpHostForward = true
			case "rootlesskit":
				slirp4netnsOpts.isSlirpHostForward = false
			default:
				return nil, errors.Errorf("unknown port_handler for slirp4netns: %q", value)
			}
		case "allow_host_loopback":
			switch value {
			case "true":
				slirp4netnsOpts.disableHostLoopback = false
			case "false":
				slirp4netnsOpts.disableHostLoopback = true
			default:
				return nil, errors.Errorf("invalid value of allow_host_loopback for slirp4netns: %q", value)
			}
		case "enable_ipv6":
			switch value {
			case "true":
				slirp4netnsOpts.enableIPv6 = true
			case "false":
				slirp4netnsOpts.enableIPv6 = false
			default:
				return nil, errors.Errorf("invalid value of enable_ipv6 for slirp4netns: %q", value)
			}
		case "outbound_addr":
			ipv4 := net.ParseIP(value)
			if ipv4 == nil || ipv4.To4() == nil {
				_, err := net.InterfaceByName(value)
				if err != nil {
					return nil, errors.Errorf("invalid outbound_addr %q", value)
				}
			}
			slirp4netnsOpts.outboundAddr = value
		case "outbound_addr6":
			ipv6 := net.ParseIP(value)
			if ipv6 == nil || ipv6.To4() != nil {
				_, err := net.InterfaceByName(value)
				if err != nil {
					return nil, errors.Errorf("invalid outbound_addr6: %q", value)
				}
			}
			slirp4netnsOpts.outboundAddr6 = value
		case "mtu":
			var err error
			slirp4netnsOpts.mtu, err = strconv.Atoi(value)
			if slirp4netnsOpts.mtu < 68 || err != nil {
				return nil, errors.Errorf("invalid mtu %q", value)
			}
		default:
			return nil, errors.Errorf("unknown option for slirp4netns: %q", o)
		}
	}
	return slirp4netnsOpts, nil
}

func createBasicSlirp4netnsCmdArgs(options *slirp4netnsNetworkOptions, features *slirpFeatures) ([]string, error) {
	cmdArgs := []string{}
	if options.disableHostLoopback && features.HasDisableHostLoopback {
		cmdArgs = append(cmdArgs, "--disable-host-loopback")
	}
	if options.mtu > -1 && features.HasMTU {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--mtu=%d", options.mtu))
	}
	if !options.noPivotRoot && features.HasEnableSandbox {
		cmdArgs = append(cmdArgs, "--enable-sandbox")
	}
	if features.HasEnableSeccomp {
		cmdArgs = append(cmdArgs, "--enable-seccomp")
	}

	if options.cidr != "" {
		if !features.HasCIDR {
			return nil, errors.Errorf("cidr not supported")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--cidr=%s", options.cidr))
	}

	if options.enableIPv6 {
		if !features.HasIPv6 {
			return nil, errors.Errorf("enable_ipv6 not supported")
		}
		cmdArgs = append(cmdArgs, "--enable-ipv6")
	}

	if options.outboundAddr != "" {
		if !features.HasOutboundAddr {
			return nil, errors.Errorf("outbound_addr not supported")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--outbound-addr=%s", options.outboundAddr))
	}

	if options.outboundAddr6 != "" {
		if !features.HasOutboundAddr || !features.HasIPv6 {
			return nil, errors.Errorf("outbound_addr6 not supported")
		}
		if !options.enableIPv6 {
			return nil, errors.Errorf("enable_ipv6=true is required for outbound_addr6")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--outbound-addr6=%s", options.outboundAddr6))
	}

	return cmdArgs, nil
}

// setupSlirp4netns can be called in rootful as well as in rootless
func (r *Runtime) setupSlirp4netns(ctr *Container) error {
	path := r.config.Engine.NetworkCmdPath
	if path == "" {
		var err error
		path, err = exec.LookPath("slirp4netns")
		if err != nil {
			logrus.Errorf("could not find slirp4netns, the network namespace won't be configured: %v", err)
			return nil
		}
	}

	syncR, syncW, err := os.Pipe()
	if err != nil {
		return errors.Wrapf(err, "failed to open pipe")
	}
	defer errorhandling.CloseQuiet(syncR)
	defer errorhandling.CloseQuiet(syncW)

	havePortMapping := len(ctr.Config().PortMappings) > 0
	logPath := filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("slirp4netns-%s.log", ctr.config.ID))

	ctrNetworkSlipOpts := []string{}
	if ctr.config.NetworkOptions != nil {
		ctrNetworkSlipOpts = append(ctrNetworkSlipOpts, ctr.config.NetworkOptions["slirp4netns"]...)
	}
	netOptions, err := parseSlirp4netnsNetworkOptions(r, ctrNetworkSlipOpts)
	if err != nil {
		return err
	}
	slirpFeatures, err := checkSlirpFlags(path)
	if err != nil {
		return errors.Wrapf(err, "error checking slirp4netns binary %s: %q", path, err)
	}
	cmdArgs, err := createBasicSlirp4netnsCmdArgs(netOptions, slirpFeatures)
	if err != nil {
		return err
	}

	// the slirp4netns arguments being passed are describes as follows:
	// from the slirp4netns documentation: https://github.com/rootless-containers/slirp4netns
	// -c, --configure Brings up the tap interface
	// -e, --exit-fd=FD specify the FD for terminating slirp4netns
	// -r, --ready-fd=FD specify the FD to write to when the initialization steps are finished
	cmdArgs = append(cmdArgs, "-c", "-e", "3", "-r", "4")

	var apiSocket string
	if havePortMapping && netOptions.isSlirpHostForward {
		apiSocket = filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("%s.net", ctr.config.ID))
		cmdArgs = append(cmdArgs, "--api-socket", apiSocket)
	}
	netnsPath := ""
	if !ctr.config.PostConfigureNetNS {
		ctr.rootlessSlirpSyncR, ctr.rootlessSlirpSyncW, err = os.Pipe()
		if err != nil {
			return errors.Wrapf(err, "failed to create rootless network sync pipe")
		}
		netnsPath = ctr.state.NetNS.Path()
		cmdArgs = append(cmdArgs, "--netns-type=path", netnsPath, "tap0")
	} else {
		defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncR)
		defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncW)
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", ctr.state.PID)
		// we don't use --netns-path here (unavailable for slirp4netns < v0.4)
		cmdArgs = append(cmdArgs, fmt.Sprintf("%d", ctr.state.PID), "tap0")
	}

	cmd := exec.Command(path, cmdArgs...)
	logrus.Debugf("slirp4netns command: %s", strings.Join(cmd.Args, " "))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// workaround for https://github.com/rootless-containers/slirp4netns/pull/153
	if !netOptions.noPivotRoot && slirpFeatures.HasEnableSandbox {
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNS
		cmd.SysProcAttr.Unshareflags = syscall.CLONE_NEWNS
	}

	// Leak one end of the pipe in slirp4netns, the other will be sent to conmon
	cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessSlirpSyncR, syncW)

	logFile, err := os.Create(logPath)
	if err != nil {
		return errors.Wrapf(err, "failed to open slirp4netns log file %s", logPath)
	}
	defer logFile.Close()
	// Unlink immediately the file so we won't need to worry about cleaning it up later.
	// It is still accessible through the open fd logFile.
	if err := os.Remove(logPath); err != nil {
		return errors.Wrapf(err, "delete file %s", logPath)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "failed to start slirp4netns process")
	}
	defer func() {
		if err := cmd.Process.Release(); err != nil {
			logrus.Errorf("unable to release command process: %q", err)
		}
	}()

	if err := waitForSync(syncR, cmd, logFile, 1*time.Second); err != nil {
		return err
	}

	if havePortMapping {
		if netOptions.isSlirpHostForward {
			return r.setupRootlessPortMappingViaSlirp(ctr, cmd, apiSocket)
		}
		return r.setupRootlessPortMappingViaRLK(ctr, netnsPath, netOptions.cidr)
	}
	return nil
}

func waitForSync(syncR *os.File, cmd *exec.Cmd, logFile io.ReadSeeker, timeout time.Duration) error {
	prog := filepath.Base(cmd.Path)
	if len(cmd.Args) > 0 {
		prog = cmd.Args[0]
	}
	b := make([]byte, 16)
	for {
		if err := syncR.SetDeadline(time.Now().Add(timeout)); err != nil {
			return errors.Wrapf(err, "error setting %s pipe timeout", prog)
		}
		// FIXME: return err as soon as proc exits, without waiting for timeout
		if _, err := syncR.Read(b); err == nil {
			break
		} else {
			if os.IsTimeout(err) {
				// Check if the process is still running.
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
				if err != nil {
					return errors.Wrapf(err, "failed to read %s process status", prog)
				}
				if pid != cmd.Process.Pid {
					continue
				}
				if status.Exited() {
					// Seek at the beginning of the file and read all its content
					if _, err := logFile.Seek(0, 0); err != nil {
						logrus.Errorf("could not seek log file: %q", err)
					}
					logContent, err := ioutil.ReadAll(logFile)
					if err != nil {
						return errors.Wrapf(err, "%s failed", prog)
					}
					return errors.Errorf("%s failed: %q", prog, logContent)
				}
				if status.Signaled() {
					return errors.Errorf("%s killed by signal", prog)
				}
				continue
			}
			return errors.Wrapf(err, "failed to read from %s sync pipe", prog)
		}
	}
	return nil
}

func (r *Runtime) setupRootlessPortMappingViaRLK(ctr *Container, netnsPath, slirp4CIDR string) error {
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return errors.Wrapf(err, "failed to open pipe")
	}
	defer errorhandling.CloseQuiet(syncR)
	defer errorhandling.CloseQuiet(syncW)

	logPath := filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("rootlessport-%s.log", ctr.config.ID))
	logFile, err := os.Create(logPath)
	if err != nil {
		return errors.Wrapf(err, "failed to open rootlessport log file %s", logPath)
	}
	defer logFile.Close()
	// Unlink immediately the file so we won't need to worry about cleaning it up later.
	// It is still accessible through the open fd logFile.
	if err := os.Remove(logPath); err != nil {
		return errors.Wrapf(err, "delete file %s", logPath)
	}

	if !ctr.config.PostConfigureNetNS {
		ctr.rootlessPortSyncR, ctr.rootlessPortSyncW, err = os.Pipe()
		if err != nil {
			return errors.Wrapf(err, "failed to create rootless port sync pipe")
		}
	}

	childIP := slirp4netnsIP
	// set the correct childIP when a custom cidr is set
	if slirp4CIDR != "" {
		_, cidr, err := net.ParseCIDR(slirp4CIDR)
		if err != nil {
			return errors.Wrap(err, "failed to parse slirp4netns cidr")
		}
		// the slirp container ip is always the hundredth ip in the subnet
		cidr.IP[len(cidr.IP)-1] = cidr.IP[len(cidr.IP)-1] + 100
		childIP = cidr.IP.String()
	}
outer:
	for _, r := range ctr.state.NetworkStatus {
		for _, i := range r.IPs {
			ipv4 := i.Address.IP.To4()
			if ipv4 != nil {
				childIP = ipv4.String()
				break outer
			}
		}
	}

	cfg := rootlessport.Config{
		Mappings:  ctr.config.PortMappings,
		NetNSPath: netnsPath,
		ExitFD:    3,
		ReadyFD:   4,
		TmpDir:    ctr.runtime.config.Engine.TmpDir,
		ChildIP:   childIP,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	cfgR := bytes.NewReader(cfgJSON)
	var stdout bytes.Buffer
	cmd := exec.Command(fmt.Sprintf("/proc/%d/exe", os.Getpid()))
	cmd.Args = []string{rootlessport.ReexecKey}
	// Leak one end of the pipe in rootlessport process, the other will be sent to conmon

	if ctr.rootlessPortSyncR != nil {
		defer errorhandling.CloseQuiet(ctr.rootlessPortSyncR)
	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessPortSyncR, syncW)
	cmd.Stdin = cfgR
	// stdout is for human-readable error, stderr is for debug log
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(logFile, &logrusDebugWriter{"rootlessport: "})
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "failed to start rootlessport process")
	}
	defer func() {
		if err := cmd.Process.Release(); err != nil {
			logrus.Errorf("unable to release rootlessport process: %q", err)
		}
	}()
	if err := waitForSync(syncR, cmd, logFile, 3*time.Second); err != nil {
		stdoutStr := stdout.String()
		if stdoutStr != "" {
			// err contains full debug log and too verbose, so return stdoutStr
			logrus.Debug(err)
			return errors.Errorf("rootlessport " + strings.TrimSuffix(stdoutStr, "\n"))
		}
		return err
	}
	logrus.Debug("rootlessport is ready")
	return nil
}

func (r *Runtime) setupRootlessPortMappingViaSlirp(ctr *Container, cmd *exec.Cmd, apiSocket string) (err error) {
	const pidWaitTimeout = 60 * time.Second
	chWait := make(chan error)
	go func() {
		interval := 25 * time.Millisecond
		for i := time.Duration(0); i < pidWaitTimeout; i += interval {
			// Check if the process is still running.
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
			if err != nil {
				break
			}
			if pid != cmd.Process.Pid {
				continue
			}
			if status.Exited() || status.Signaled() {
				chWait <- fmt.Errorf("slirp4netns exited with status %d", status.ExitStatus())
			}
			time.Sleep(interval)
		}
	}()
	defer close(chWait)

	// wait that API socket file appears before trying to use it.
	if _, err := WaitForFile(apiSocket, chWait, pidWaitTimeout); err != nil {
		return errors.Wrapf(err, "waiting for slirp4nets to create the api socket file %s", apiSocket)
	}

	// for each port we want to add we need to open a connection to the slirp4netns control socket
	// and send the add_hostfwd command.
	for _, i := range ctr.config.PortMappings {
		conn, err := net.Dial("unix", apiSocket)
		if err != nil {
			return errors.Wrapf(err, "cannot open connection to %s", apiSocket)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				logrus.Errorf("unable to close connection: %q", err)
			}
		}()
		hostIP := i.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		apiCmd := slirp4netnsCmd{
			Execute: "add_hostfwd",
			Args: slirp4netnsCmdArg{
				Proto:     i.Protocol,
				HostAddr:  hostIP,
				HostPort:  i.HostPort,
				GuestPort: i.ContainerPort,
			},
		}
		// create the JSON payload and send it.  Mark the end of request shutting down writes
		// to the socket, as requested by slirp4netns.
		data, err := json.Marshal(&apiCmd)
		if err != nil {
			return errors.Wrapf(err, "cannot marshal JSON for slirp4netns")
		}
		if _, err := conn.Write([]byte(fmt.Sprintf("%s\n", data))); err != nil {
			return errors.Wrapf(err, "cannot write to control socket %s", apiSocket)
		}
		if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
			return errors.Wrapf(err, "cannot shutdown the socket %s", apiSocket)
		}
		buf := make([]byte, 2048)
		readLength, err := conn.Read(buf)
		if err != nil {
			return errors.Wrapf(err, "cannot read from control socket %s", apiSocket)
		}
		// if there is no 'error' key in the received JSON data, then the operation was
		// successful.
		var y map[string]interface{}
		if err := json.Unmarshal(buf[0:readLength], &y); err != nil {
			return errors.Wrapf(err, "error parsing error status from slirp4netns")
		}
		if e, found := y["error"]; found {
			return errors.Errorf("error from slirp4netns while setting up port redirection: %v", e)
		}
	}
	logrus.Debug("slirp4netns port-forwarding setup via add_hostfwd is ready")
	return nil
}
