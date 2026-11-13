package config

import "testing"

func daemon() *Daemon {
	return &Daemon{
		DataRoot:     "/var/lib/docker",
		Driver:       "overlay2",
		DriverStatus: [][2]string{{"Backing Filesystem", "extfs"}},
		Runtimes:     []string{"runc", "io.containerd.runc.v2"},
	}
}

func TestCheckDaemonAccepts(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, tokenEnv(), withProfile("mounts: [{bind: /srv/cache, target: /cache}], scopes: [acme/app]"))
	if err := cfg.CheckDaemon(daemon()); err != nil {
		t.Fatalf("CheckDaemon: %v", err)
	}
}

func TestCheckDaemonRuntime(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, tokenEnv(), withProfile("runtime: sysbox-runc, scopes: [acme/app]"))
	wantErr(t, cfg.CheckDaemon(daemon()), `profiles.p.runtime: the Docker daemon has no runtime "sysbox-runc"`)
	d := daemon()
	d.Runtimes = append(d.Runtimes, "sysbox-runc")
	if err := cfg.CheckDaemon(d); err != nil {
		t.Errorf("CheckDaemon with Sysbox: %v", err)
	}
}

func TestCheckDaemonDisk(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, tokenEnv(), withProfile("disk: 20GiB"))
	for _, tc := range []struct {
		name, driver, want string
		status             [][2]string
	}{
		{"zfs", "zfs", "", nil},
		{"btrfs", "btrfs", "", nil},
		{"overlayOnXFS", "overlay2", "", [][2]string{{"Backing Filesystem", "xfs"}}},
		{"overlayOnExt4", "overlay2", "does not enforce a disk limit", [][2]string{{"Backing Filesystem", "extfs"}}},
		{"overlayOnZFS", "overlay2", "does not enforce a disk limit", [][2]string{{"Backing Filesystem", "zfs"}}},
		{"vfs", "vfs", `storage driver "vfs"`, nil},
		{"containerdStore", "overlayfs", "the containerd image store ignores a disk limit", [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := daemon()
			d.Driver, d.DriverStatus = tc.driver, tc.status
			err := cfg.CheckDaemon(d)
			if tc.want == "" {
				if err != nil {
					t.Errorf("CheckDaemon: %v", err)
				}
				return
			}
			wantErr(t, err, "profiles.p.disk: ", tc.want)
		})
	}
	t.Run("noDiskAnyDriver", func(t *testing.T) {
		t.Parallel()
		d := daemon()
		d.Driver = "vfs"
		if err := mustBuild(t, tokenEnv(), withProfile("")).CheckDaemon(d); err != nil {
			t.Errorf("CheckDaemon without disk: %v", err)
		}
	})
}

func TestCheckDaemonProtectedBinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, bind, dataRoot, containerd, want string }{
		{"socket", "/var/run/docker.sock", "/var/lib/docker", "", "would reach /var/run/docker.sock"},
		{"socketRunForm", "/run/docker.sock", "/var/lib/docker", "", "would reach /run/docker.sock"},
		{"socketAncestorVarRun", "/var/run", "/var/lib/docker", "", "/var/run"},
		{"socketAncestorRun", "/run", "/var/lib/docker", "", "/run"},
		{"socketAncestorVar", "/var", "/var/lib/docker", "", "/var"},
		{"root", "/", "/var/lib/docker", "", "would reach"},
		{"varLibAncestor", "/var/lib", "/var/lib/docker", "", "/var/lib/containerd"},
		{"containerdRoot", "/var/lib/containerd", "/var/lib/docker", "", "/var/lib/containerd"},
		{"containerdSocket", "/run/containerd/containerd.sock", "/var/lib/docker", "", "/run/containerd"},
		{"dockerExecRoot", "/var/run/docker", "/var/lib/docker", "", "/var/run/docker"},
		{"proc", "/proc/1", "/var/lib/docker", "", "/proc"},
		{"sys", "/sys", "/var/lib/docker", "", "/sys"},
		{"dataRoot", "/var/lib/docker", "/var/lib/docker", "", "/var/lib/docker"},
		{"underDataRoot", "/var/lib/docker/volumes/x", "/var/lib/docker", "", "/var/lib/docker"},
		{"dataRootAncestor", "/srv", "/srv/docker", "", "/srv/docker"},
		{"reportedContainerdSocket", "/opt/containerd", "/var/lib/docker", "/opt/containerd/containerd.sock", "/opt/containerd/containerd.sock"},
		{"reportedSocketRunForm", "/run/ctr", "/var/lib/docker", "/var/run/ctr/containerd.sock", "/run/ctr/containerd.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustBuild(t, tokenEnv(), withProfile("mounts: [{bind: "+tc.bind+", target: /x}], scopes: [acme/app]"))
			d := daemon()
			d.DataRoot, d.ContainerdAddress = tc.dataRoot, tc.containerd
			wantErr(t, cfg.CheckDaemon(d), "profiles.p.mounts[0].bind:", tc.want)
		})
	}
	t.Run("siblingNameIsNoAncestor", func(t *testing.T) {
		t.Parallel()
		cfg := mustBuild(t, tokenEnv(), withProfile("mounts: [{bind: /procfs, target: /x}, {bind: /var/lib/dockerd, target: /y}], scopes: [acme/app]"))
		if err := cfg.CheckDaemon(daemon()); err != nil {
			t.Errorf("CheckDaemon: %v", err)
		}
	})
	t.Run("customSocket", func(t *testing.T) {
		t.Parallel()
		env := tokenEnv()
		env.DockerSocket = "/srv/docker/docker.sock"
		cfg := mustBuild(t, env, withProfile("mounts: [{bind: /srv/docker, target: /s}], scopes: [acme/app]"))
		wantErr(t, cfg.CheckDaemon(daemon()), "/srv/docker/docker.sock")
	})
}
