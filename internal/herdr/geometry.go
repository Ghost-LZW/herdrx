package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/riba2534/herdrx/internal/terminalgeometry"
)

func terminalPID(ctx context.Context, endpoint Endpoint, paneID string) (int, error) {
	if !publicID.MatchString(paneID) {
		return 0, fmt.Errorf("invalid pane id")
	}
	result, err := endpoint.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return 0, err
	}
	var reply struct {
		Info struct {
			PaneID string `json:"pane_id"`
			PID    int    `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(result, &reply); err != nil {
		return 0, fmt.Errorf("decode terminal process: %w", err)
	}
	if reply.Info.PaneID != paneID || reply.Info.PID <= 1 || reply.Info.PID > 1<<30 {
		return 0, fmt.Errorf("terminal process is unavailable")
	}
	return reply.Info.PID, nil
}

func terminalSocketPath(apiPath string) (string, error) {
	if !strings.HasSuffix(apiPath, "/herdr.sock") {
		return "", fmt.Errorf("unknown Herdr socket layout")
	}
	return strings.TrimSuffix(apiPath, "herdr.sock") + "herdr-client.sock", nil
}

func (e *LocalEndpoint) TerminalGeometry(ctx context.Context, paneID string) (TerminalGeometry, error) {
	pid, err := terminalPID(ctx, e, paneID)
	if err != nil {
		return TerminalGeometry{}, err
	}
	if err := ctx.Err(); err != nil {
		return TerminalGeometry{}, err
	}
	return terminalgeometry.ReadPID(pid)
}

func (e *LocalEndpoint) OpenTerminalSocket(ctx context.Context) (net.Conn, error) {
	path, err := terminalSocketPath(e.SocketPath)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// This fixed program only opens the pane process's terminal and performs
// TIOCGWINSZ. The validated integer PID is a separate argument; no remote data
// becomes code. Linux reads stdin's terminal through procfs. macOS has no
// procfs, so it follows terminalgeometry.ReadPID on darwin: /bin/ps reports the
// controlling-terminal device the kernel records as major/minor ("??" when there
// is none), the matching ptmx slave /dev/ttysNNN is opened, and the opened fd
// must be that character device before its size is trusted. The program is sent
// inside single quotes, so it must not contain one.
const remoteGeometryProgram = `import os,sys,fcntl,termios,struct,json,stat,subprocess
pid=int(sys.argv[1])
if not 1 < pid <= 1073741824: raise RuntimeError("invalid terminal process id")
if sys.platform == "linux": fd=os.open("/proc/%d/fd/0" % pid,os.O_RDONLY|os.O_NOCTTY|os.O_NONBLOCK)
elif sys.platform == "darwin":
 tdev=subprocess.check_output(["/bin/ps","-o","tdev=","-p",str(pid)]).decode().strip()
 if "/" not in tdev: raise RuntimeError("process has no controlling terminal")
 major,minor=(int(part) for part in tdev.split("/"))
 fd=os.open("/dev/ttys%03d" % minor,os.O_RDONLY|os.O_NOCTTY|os.O_NONBLOCK)
 info=os.fstat(fd)
 if not stat.S_ISCHR(info.st_mode) or info.st_rdev != os.makedev(major,minor): os.close(fd); raise RuntimeError("terminal size source is not the controlling terminal")
else: raise RuntimeError("preserving terminal geometry currently requires a Linux or macOS Herdr host")
try: rows,cols,width,height=struct.unpack("HHHH",fcntl.ioctl(fd,termios.TIOCGWINSZ,bytes(8)))
finally: os.close(fd)
if not (10<=cols<=1000 and 3<=rows<=500) or width==65535 or height==65535 or width%cols or height%rows: raise RuntimeError("terminal size cannot be preserved exactly")
print(json.dumps(dict(cols=cols,rows=rows,cell_width_px=width//cols,cell_height_px=height//rows)))`

func (e *SSHEndpoint) TerminalGeometry(ctx context.Context, paneID string) (TerminalGeometry, error) {
	pid, err := terminalPID(ctx, e, paneID)
	if err != nil {
		return TerminalGeometry{}, err
	}
	session, err := e.newSession(ctx)
	if err != nil {
		return TerminalGeometry{}, err
	}
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	command := fmt.Sprintf("python3 -c '%s' %d", remoteGeometryProgram, pid)
	if e.host.Transport == "tailcat" {
		command = fmt.Sprintf("herdrx-terminal-geometry %d", pid)
	}
	output, err := session.Output(command)
	if err != nil {
		if ctx.Err() != nil {
			return TerminalGeometry{}, ctx.Err()
		}
		if e.host.Transport == "tailcat" {
			return TerminalGeometry{}, fmt.Errorf("无法读取远程终端尺寸，请更新远程主机上的 herdrx CLI 后重试: %w", err)
		}
		return TerminalGeometry{}, fmt.Errorf("无法读取远程终端尺寸，请确认 Herdr 主机为 Linux 或 macOS 且已安装 Python 3: %w", err)
	}
	var geometry TerminalGeometry
	if err := json.Unmarshal(output, &geometry); err != nil {
		return geometry, fmt.Errorf("decode remote terminal geometry: %w", err)
	}
	return geometry, geometry.Validate()
}

func (e *SSHEndpoint) OpenTerminalSocket(ctx context.Context) (net.Conn, error) {
	path, err := terminalSocketPath(e.socketPath)
	if err != nil {
		return nil, err
	}
	return e.client.DialContext(ctx, "unix", path)
}
