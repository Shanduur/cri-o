package lib

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/checkpoint-restore/go-criu/v5/stats"
	"github.com/containers/podman/v4/libpod"
	"github.com/containers/podman/v4/pkg/annotations"
	"github.com/containers/podman/v4/pkg/checkpoint/crutils"
	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/internal/oci"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/sirupsen/logrus"
)

type ContainerCheckpointRestoreOptions struct {
	Container string
	Pod       string

	libpod.ContainerCheckpointOptions
}

// ContainerCheckpoint checkpoints a running container.
func (c *ContainerServer) ContainerCheckpoint(ctx context.Context, opts *ContainerCheckpointRestoreOptions) (string, error) {
	ctr, err := c.LookupContainer(opts.Container)
	if err != nil {
		return "", fmt.Errorf("failed to find container %s: %w", opts.Container, err)
	}

	configFile := filepath.Join(ctr.BundlePath(), "config.json")
	specgen, err := generate.NewFromFile(configFile)
	if err != nil {
		return "", fmt.Errorf("not able to read config for container %q: %w", ctr.ID(), err)
	}

	cStatus := ctr.State()
	if cStatus.Status != oci.ContainerStateRunning {
		return "", fmt.Errorf("container %s is not running", ctr.ID())
	}

	if opts.TargetFile != "" {
		if err := c.prepareCheckpointExport(ctr); err != nil {
			return "", fmt.Errorf("failed to write config dumps for container %s: %w", ctr.ID(), err)
		}
	}

	if err := c.runtime.CheckpointContainer(ctx, ctr, specgen.Config, opts.KeepRunning); err != nil {
		return "", fmt.Errorf("failed to checkpoint container %s: %w", ctr.ID(), err)
	}
	if opts.TargetFile != "" {
		if err := c.exportCheckpoint(ctr, specgen.Config, opts.TargetFile); err != nil {
			return "", fmt.Errorf("failed to write file system changes of container %s: %w", ctr.ID(), err)
		}
	}
	if err := c.storageRuntimeServer.StopContainer(ctr.ID()); err != nil {
		return "", fmt.Errorf("failed to unmount container %s: %w", ctr.ID(), err)
	}
	if err := c.ContainerStateToDisk(ctx, ctr); err != nil {
		logrus.Warnf("Unable to write containers %s state to disk: %v", ctr.ID(), err)
	}

	if !opts.Keep {
		cleanup := []string{
			metadata.DumpLogFile,
			stats.StatsDump,
			metadata.ConfigDumpFile,
			metadata.SpecDumpFile,
		}
		for _, del := range cleanup {
			file := filepath.Join(ctr.Dir(), del)
			if err := os.Remove(file); err != nil {
				logrus.Debugf("Unable to remove file %s", file)
			}
		}
	}

	return ctr.ID(), nil
}

// Copied from libpod/diff.go
var containerMounts = map[string]bool{
	"/dev":               true,
	"/dev/shm":           true,
	"/proc":              true,
	"/run":               true,
	"/run/.containerenv": true,
	"/run/secrets":       true,
	"/sys":               true,
}

const bindMount = "bind"

func skipBindMount(mountPath string, specgen *rspec.Spec) bool {
	for _, m := range specgen.Mounts {
		if m.Type != bindMount {
			continue
		}
		if m.Destination == mountPath {
			return true
		}
	}

	return false
}

// getDiff returns the file system differences
// Copied from libpod/diff.go and simplified for the checkpoint use case
func (c *ContainerServer) getDiff(id string, specgen *rspec.Spec) (rchanges []archive.Change, err error) {
	layerID, err := c.GetContainerTopLayerID(id)
	if err != nil {
		return nil, err
	}
	changes, err := c.store.Changes("", layerID)
	if err == nil {
		for _, c := range changes {
			if skipBindMount(c.Path, specgen) {
				continue
			}
			if containerMounts[c.Path] {
				continue
			}
			rchanges = append(rchanges, c)
		}
	}
	return rchanges, err
}

type ExternalBindMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	FileType    string `json:"file_type"`
	Permissions uint32 `json:"permissions"`
}

// prepareCheckpointExport writes the config and spec to
// JSON files for later export
// Podman: libpod/container_internal.go
func (c *ContainerServer) prepareCheckpointExport(ctr *oci.Container) error {
	// save spec
	jsonPath := filepath.Join(ctr.BundlePath(), "config.json")
	g, err := generate.NewFromFile(jsonPath)
	if err != nil {
		return fmt.Errorf("generating spec for container %q failed: %w", ctr.ID(), err)
	}
	if _, err := metadata.WriteJSONFile(g.Config, ctr.Dir(), metadata.SpecDumpFile); err != nil {
		return fmt.Errorf("generating spec for container %q failed: %w", ctr.ID(), err)
	}

	config := &metadata.ContainerConfig{
		ID:              ctr.ID(),
		Name:            ctr.Name(),
		RootfsImageName: ctr.ImageName(),
		CreatedTime:     ctr.CreatedAt(),
		OCIRuntime: func() string {
			runtimeHandler := c.GetSandbox(ctr.Sandbox()).RuntimeHandler()
			if runtimeHandler != "" {
				return runtimeHandler
			}
			return c.config.DefaultRuntime
		}(),
	}

	if _, err := metadata.WriteJSONFile(config, ctr.Dir(), metadata.ConfigDumpFile); err != nil {
		return err
	}

	// During container creation CRI-O creates all missing bind mount sources as
	// directories. This is disabled during restore as CRIU requires the bind mount
	// source to be of the same type. Directories need to be directories and regular
	// files need to be regular files. CRIU will fail to bind mount a directory on
	// a file. Especiallay when restoring a Kubernetes container outside of Kubernetes
	// a couple of bind mounts are files (e.g. /etc/resolv.conf). To solve this
	// CRI-O is now tracking all bind mount types in the checkpoint archive. This
	// way it is possible to know if a missing bind mount needs to be a file or a
	// directory.
	var externalBindMounts []ExternalBindMount //nolint:prealloc
	for _, m := range g.Config.Mounts {
		if containerMounts[m.Destination] {
			continue
		}
		if m.Type != bindMount {
			continue
		}
		fileInfo, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("unable to stat() %q: %w", m.Source, err)
		}

		externalBindMounts = append(
			externalBindMounts,
			ExternalBindMount{
				Source:      m.Source,
				Destination: m.Destination,
				FileType: func() string {
					if fileInfo.Mode().IsDir() {
						return "directory"
					}
					return "file"
				}(),
				Permissions: uint32(fileInfo.Mode().Perm()),
			},
		)
	}

	if len(externalBindMounts) > 0 {
		if _, err := metadata.WriteJSONFile(externalBindMounts, ctr.Dir(), "bind.mounts"); err != nil {
			return fmt.Errorf("error writing 'bind.mounts' for %q: %w", ctr.ID(), err)
		}
	}

	return nil
}

func (c *ContainerServer) exportCheckpoint(ctr *oci.Container, specgen *rspec.Spec, export string) error {
	id := ctr.ID()
	dest := ctr.Dir()
	logrus.Debugf("Exporting checkpoint image of container %q to %q", id, dest)

	includeFiles := []string{
		stats.StatsDump,
		metadata.DumpLogFile,
		metadata.CheckpointDirectory,
		metadata.ConfigDumpFile,
		metadata.SpecDumpFile,
		"bind.mounts",
	}

	// To correctly track deleted files, let's go through the output of 'podman diff'
	rootFsChanges, err := c.getDiff(id, specgen)
	if err != nil {
		return fmt.Errorf("error exporting root file-system diff for %q: %w", id, err)
	}
	mountPoint, err := c.StorageImageServer().GetStore().Mount(id, specgen.Linux.MountLabel)
	if err != nil {
		return fmt.Errorf("not able to get mountpoint for container %q: %w", id, err)
	}
	addToTarFiles, err := crutils.CRCreateRootFsDiffTar(&rootFsChanges, mountPoint, dest)
	if err != nil {
		return err
	}

	// Put log file into checkpoint archive
	_, err = os.Stat(specgen.Annotations[annotations.LogPath])
	if err == nil {
		src, err := os.Open(specgen.Annotations[annotations.LogPath])
		if err != nil {
			return fmt.Errorf("error opening log file %q: %w", specgen.Annotations[annotations.LogPath], err)
		}
		defer src.Close()
		destLogPath := filepath.Join(dest, annotations.LogPath)
		destLog, err := os.Create(destLogPath)
		if err != nil {
			return fmt.Errorf("error opening log file %q: %w", destLogPath, err)
		}
		defer destLog.Close()
		_, err = io.Copy(destLog, src)
		if err != nil {
			return fmt.Errorf("copying log file to %q failed: %w", destLogPath, err)
		}
		addToTarFiles = append(addToTarFiles, annotations.LogPath)
	}

	includeFiles = append(includeFiles, addToTarFiles...)

	input, err := archive.TarWithOptions(ctr.Dir(), &archive.TarOptions{
		// This should be configurable via api.proti
		Compression:      archive.Uncompressed,
		IncludeSourceDir: true,
		IncludeFiles:     includeFiles,
	})
	if err != nil {
		return fmt.Errorf("error reading checkpoint directory %q: %w", id, err)
	}

	// The resulting tar archive should not be readable by everyone as it contains
	// every memory page of the checkpointed processes.
	outFile, err := os.OpenFile(export, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("error creating checkpoint export file %q: %w", export, err)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, input)
	if err != nil {
		return err
	}

	for _, file := range addToTarFiles {
		os.Remove(filepath.Join(dest, file))
	}

	return nil
}
