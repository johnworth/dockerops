package dockerops

import (
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"context"

	"github.com/cyverse-de/logcabin"
	"github.com/cyverse-de/model"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	nat "github.com/docker/go-connections/nat"
	"github.com/spf13/viper"
)

// Docker provides operations that runner needs from the docker client.
type Docker struct {
	Client        *client.Client
	TransferImage string
	cfg           *viper.Viper
	ctx           context.Context
}

// WORKDIR is the path to the working directory inside all of the containers
// that are run as part of a job.
const WORKDIR = "/de-app-work"

// CONFIGDIR is the path to the local configs inside the containers that are
// used to transfer files into and out of the job.
const CONFIGDIR = "/configs"

// VOLUMEDIR is the name of the directory that is used for the working directory
// volume.
const VOLUMEDIR = "workingvolume"

const (
	// TypeLabel is the label key applied to every container.
	TypeLabel = "org.iplantc.containertype"

	// InputContainer is the value used in the TypeLabel for input containers.
	InputContainer = iota

	// DataContainer is the value used in the TypeLabel for data containers.
	DataContainer

	// StepContainer is the value used in the TypeLabel for step containers.
	StepContainer

	// OutputContainer is the value used in the TypeLabel for output containers.
	OutputContainer
)

// NewDocker returns a *Docker that connects to the docker client listening at
// 'uri'.
func NewDocker(ctx context.Context, cfg *viper.Viper, uri string) (*Docker, error) {
	defaultHeaders := map[string]string{"User-Agent": "cyverse-road-runner-1.0"}
	cl, err := client.NewClient(uri, "v1.23", nil, defaultHeaders)
	if err != nil {
		return nil, err
	}
	d := &Docker{
		Client: cl,
		cfg:    cfg,
		ctx:    ctx,
	}
	return d, err
}

// IsContainer returns true if the provided 'name' is a container on the system
func (d *Docker) IsContainer(name string) (bool, error) {
	opts := types.ContainerListOptions{All: true}
	list, err := d.Client.ContainerList(d.ctx, opts)
	if err != nil {
		return false, err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// IsRunning returns true if the contain with 'name' is running.
func (d *Docker) IsRunning(name string) (bool, error) {
	opts := types.ContainerListOptions{}
	list, err := d.Client.ContainerList(d.ctx, opts)
	if err != nil {
		return false, err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// ContainersWithLabel returns the id of all containers that have the label
// "key=value" applied to it.
func (d *Docker) ContainersWithLabel(key, value string, all bool) ([]string, error) {
	f := filters.NewArgs()
	f.Add("label", fmt.Sprintf("%s=%s", key, value))
	opts := types.ContainerListOptions{
		All:     all,
		Filters: f,
	}
	list, err := d.Client.ContainerList(d.ctx, opts)
	if err != nil {
		return nil, err
	}
	var retval []string
	for _, c := range list {
		retval = append(retval, c.ID)
	}
	return retval, nil
}

// NukeContainer kills the container with the provided id.
func (d *Docker) NukeContainer(id string) error {
	fmt.Printf("Nuking container %s", id)
	return d.Client.ContainerRemove(d.ctx, id, types.ContainerRemoveOptions{
		RemoveVolumes: true,
		RemoveLinks:   false,
		Force:         true,
	})
}

// NukeContainersByLabel kills all running containers that have the provided
// label applied to them.
func (d *Docker) NukeContainersByLabel(key, value string) error {
	containers, err := d.ContainersWithLabel(key, value, false)
	if err != nil {
		return err
	}
	for _, container := range containers {
		err = d.NukeContainer(container)
		if err != nil {
			return err
		}
	}
	return nil
}

// NukeContainerByName kills and remove the named container.
func (d *Docker) NukeContainerByName(name string) error {
	list, err := d.Client.ContainerList(d.ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return err
	}
	for _, container := range list {
		for _, n := range container.Names {
			if strings.TrimPrefix(n, "/") == name {
				return d.NukeContainer(container.ID)
			}
		}
	}
	return nil
}

// ImageID returns the image ID as a string for image with the given name and tag.
func (d *Docker) ImageID(name, tag string) (string, error) {
	images, err := d.Client.ImageList(d.ctx, types.ImageListOptions{
		All: true,
	})
	if err != nil {
		return "", nil
	}
	repoTag := fmt.Sprintf("%s:%s", name, tag)
	found := ""
	for _, img := range images {
		for _, rt := range img.RepoTags {
			if rt == repoTag {
				found = img.ID
			}
		}
	}
	return found, err
}

func (d *Docker) removeImage(id string, force, prune bool) error {
	removed, err := d.Client.ImageRemove(d.ctx, id, types.ImageRemoveOptions{
		Force:         force,
		PruneChildren: prune,
	})
	if err != nil {
		return err
	}
	for _, rm := range removed {
		logcabin.Info.Printf("untagged: %s\tdeleted: %s\n", rm.Untagged, rm.Deleted)
	}
	return err
}

// SafelyRemoveImageByID will delete the image referenced by its ID.
func (d *Docker) SafelyRemoveImageByID(id string) error {
	return d.removeImage(id, false, false)
}

// InspectImage will return a types.ImageInspect instance filled out for the
// image with the provided ID.
func (d *Docker) InspectImage(id string) (types.ImageInspect, error) {
	retval, _, err := d.Client.ImageInspectWithRaw(d.ctx, id)
	return retval, err
}

// ExposedPortsForImage returns a nat.PortSet for the image with the given ID.
// Convenience function that uses InspectImage().
func (d *Docker) ExposedPortsForImage(id string) (nat.PortSet, error) {
	inspection, err := d.InspectImage(id)
	if err != nil {
		return nil, err
	}
	return inspection.Config.ExposedPorts, err
}

// SafelyRemoveImage will delete the image with force set to false
func (d *Docker) SafelyRemoveImage(name, tag string) error {
	imageID, err := d.ImageID(name, tag)
	if err != nil {
		return err
	}
	if imageID == "" {
		return fmt.Errorf("image not found: %s:%s", name, tag)
	}
	return d.SafelyRemoveImageByID(imageID)
}

// NukeImage will delete the image with force set to true.
func (d *Docker) NukeImage(name, tag string) error {
	imageID, err := d.ImageID(name, tag)
	if err != nil {
		return err
	}
	if imageID == "" {
		return fmt.Errorf("image not found: %s:%s", name, tag)
	}
	return d.removeImage(imageID, true, true)
}

// Images will returns a list of the repo tags for all the images currently
// downloaded.
func (d *Docker) Images() ([]string, error) {
	images, err := d.Client.ImageList(d.ctx, types.ImageListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var retval []string
	for _, img := range images {
		repos := img.RepoTags
		for _, r := range repos {
			retval = append(retval, r)
		}
	}
	return retval, nil
}

// DanglingImages will return a list of IDs for all dangling images.
func (d *Docker) DanglingImages() ([]string, error) {
	var err error
	imageFilter := filters.NewArgs()
	imageFilter.Add("dangling", "true")
	images, err := d.Client.ImageList(d.ctx, types.ImageListOptions{
		Filters: imageFilter,
	})
	if err != nil {
		return nil, err
	}
	var retval []string
	for _, img := range images {
		retval = append(retval, img.ID)
	}
	return retval, nil
}

func (d *Docker) basePull(name, tag string, opts types.ImagePullOptions) error {
	imageRef := fmt.Sprintf("%s:%s", name, tag)

	body, err := d.Client.ImagePull(d.ctx, imageRef, opts)
	defer body.Close()
	if err != nil {
		return err
	}

	_, err = io.Copy(os.Stdout, body)
	return err
}

// Pull will pull an image indicated by name and tag. Name is in the format
// "registry/repository". If the name doesn't contain a / then the registry
// is assumed to be "base" and the provided name will be set to repository.
// This assumes that no authentication is required.
func (d *Docker) Pull(name, tag string) error {
	return d.basePull(name, tag, types.ImagePullOptions{})
}

// PullAuthenticated is Pull, but with a third argument 'auth' which should be
// the RegistryAuth needed by docker: base64(username + ':' + password)
func (d *Docker) PullAuthenticated(name, tag, auth string) error {
	return d.basePull(name, tag, types.ImagePullOptions{
		RegistryAuth: auth,
	})
}

func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

// CreateWorkingDirVolume creates a new volume that is used to contain the
// working directory for a job.
func (d *Docker) CreateWorkingDirVolume(volumeID string) (types.Volume, error) {
	wd, err := os.Getwd()
	if err != nil {
		return types.Volume{}, err
	}

	path := path.Join(wd, VOLUMEDIR)

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			logcabin.Info.Printf("creating volume directory: %s\n", path)
			if err = os.MkdirAll(path, 0755); err != nil {
				logcabin.Info.Printf("error creating path %s: %s", path, err)
				return types.Volume{}, err
			}
		}
	}

	return d.Client.VolumeCreate(d.ctx, volume.VolumesCreateBody{
		Driver: "local",
		DriverOpts: map[string]string{
			"type":   "none",
			"device": path,
			"o":      "bind",
		},
		Name: volumeID,
	})
}

// VolumeExists return true if the volume exists.
func (d *Docker) VolumeExists(volumeID string) (bool, error) {
	list, err := d.Client.VolumeList(d.ctx, filters.NewArgs())
	if err != nil {
		return false, err
	}
	for _, l := range list.Volumes {
		if l.Name == volumeID {
			return true, nil
		}
	}
	return false, nil
}

// RemoveVolume deletes the working directory volume.
func (d *Docker) RemoveVolume(volumeID string) error {
	return d.Client.VolumeRemove(d.ctx, volumeID, true)
}

// CreateContainerFromStep creates a container from a step in the a job.
// Returns the ID of the created container.
func (d *Docker) CreateContainerFromStep(step *model.Step, invID string) (string, error) {
	config := &container.Config{}
	hostConfig := &container.HostConfig{
		Resources: container.Resources{},
	}

	if step.Component.Container.EntryPoint != "" {
		config.Entrypoint = []string{step.Component.Container.EntryPoint}
	}

	config.Cmd = step.Arguments()

	if step.Component.Container.MemoryLimit > 0 {
		hostConfig.Resources.Memory = step.Component.Container.MemoryLimit
		logcabin.Info.Printf("Memory limit is %d\n", hostConfig.Resources.Memory)
	}

	if step.Component.Container.CPUShares > 0 {
		hostConfig.Resources.CPUShares = step.Component.Container.CPUShares
		logcabin.Info.Printf("CPUShares is %d\n", hostConfig.Resources.CPUShares)
	}

	if step.Component.Container.NetworkMode != "" {
		if step.Component.Container.NetworkMode == "none" {
			config.NetworkDisabled = true
		}
		hostConfig.NetworkMode = container.NetworkMode(step.Component.Container.NetworkMode)
	}
	if !config.NetworkDisabled {
		hostConfig.PublishAllPorts = true
	}

	// Set the name of the image for the container.
	var fullName string
	if step.Component.Container.Image.Tag != "" {
		fullName = fmt.Sprintf(
			"%s:%s",
			step.Component.Container.Image.Name,
			step.Component.Container.Image.Tag,
		)
	} else {
		fullName = step.Component.Container.Image.Name
	}
	config.Image = fullName

	for _, vf := range step.Component.Container.VolumesFrom {
		hostConfig.VolumesFrom = append(
			hostConfig.VolumesFrom,
			fmt.Sprintf(
				"%s-%s",
				vf.NamePrefix,
				invID,
			),
		)
	}

	if config.Volumes == nil {
		config.Volumes = make(map[string]struct{})
	}

	// We conflated volumes and binds. declare all of the volumes
	// as volumes and only turn them into mounts if a source path
	// is also set.
	for _, vol := range step.Component.Container.Volumes {
		// declare all of the destinations as volumes
		config.Volumes[vol.ContainerPath] = struct{}{}

		// only add the volume as a mount if the HostPath is set.
		if vol.HostPath != "" {
			var rw string
			if vol.ReadOnly {
				rw = "ro"
			} else {
				rw = "rw"
			}
			hostConfig.Binds = append(
				hostConfig.Binds,
				fmt.Sprintf("%s:%s:%s", vol.HostPath, vol.ContainerPath, rw),
			)
		}
	}

	// Check to see if a working directory volume exists
	hasVolume, err := d.VolumeExists(invID)
	if err != nil {
		return "", err
	}

	// if the working directory volume exists, use it.
	if hasVolume {
		hostConfig.Binds = append(
			hostConfig.Binds,
			fmt.Sprintf("%s:%s:%s", invID, step.Component.Container.WorkingDirectory(), "rw"),
		)
	} else {
		// Otherwise, bind the local working directory into the container as the working directory.
		var wd string
		// Add the hosts working directory as a binding to the container's
		// working directory.
		wd, err = os.Getwd()
		if err != nil {
			return "", err
		}
		hostConfig.Binds = append(
			hostConfig.Binds,
			fmt.Sprintf("%s:%s:%s", wd, step.Component.Container.WorkingDirectory(), "rw"),
		)
	}

	logcabin.Info.Printf("Volumes: %#v", config.Volumes)
	logcabin.Info.Printf("Binds: %#v", hostConfig.Binds)

	// Add devices mounts to the container.
	for _, dev := range step.Component.Container.Devices {
		device := container.DeviceMapping{
			PathOnHost:        dev.HostPath,
			PathInContainer:   dev.ContainerPath,
			CgroupPermissions: dev.CgroupPermissions,
		}
		hostConfig.Devices = append(hostConfig.Devices, device)
	}

	// Set the default working directory in the container to the path defined in
	// the job JSON.
	config.WorkingDir = step.Component.Container.WorkingDirectory()

	for k, v := range step.Environment {
		config.Env = append(config.Env, fmt.Sprintf("%s=%s", k, v))
	}

	config.Labels = make(map[string]string)
	config.Labels[model.DockerLabelKey] = invID
	config.Labels[TypeLabel] = strconv.Itoa(StepContainer)

	hostConfig.LogConfig = container.LogConfig{Type: "none"}
	containerName := step.Component.Container.Name

	logcabin.Info.Printf("hostconfig: %#v\n", hostConfig)
	logcabin.Info.Printf("config: %#v\n", config)

	response, err := d.Client.ContainerCreate(d.ctx, config, hostConfig, nil, containerName)
	if err == nil {
		logcabin.Info.Printf("created container %s", response.ID)
		for _, warning := range response.Warnings {
			logcabin.Info.Printf("Warning creating %s: %s", response.ID, warning)
		}
	}
	return response.ID, err
}

// Attach will attach to a container and copy the stream output to writer. Returns an exit channel..
func (d *Docker) Attach(containerID string, outputWriter, errorWriter io.Writer) error {
	resp, err := d.Client.ContainerAttach(
		d.ctx,
		containerID,
		types.ContainerAttachOptions{
			Stream: true,
			Stdout: true,
			Stderr: true,
		},
	)

	if err != nil {
		return err
	}

	go func() {
		defer resp.Close()
		var err error
		if _, err = stdcopy.StdCopy(outputWriter, errorWriter, resp.Reader); err != nil {
			logcabin.Error.Print(err)
		}
	}()

	return nil
}

func (d *Docker) runContainer(containerID string, stdout, stderr io.Writer) (int64, error) {
	var err error

	if err = d.Attach(containerID, stdout, stderr); err != nil {
		return -1, err
	}

	//run the container
	if err = d.Client.ContainerStart(d.ctx, containerID, types.ContainerStartOptions{}); err != nil {
		return -1, err
	}

	//wait for container to exit
	return d.Client.ContainerWait(d.ctx, containerID)
}

// InspectContainer returns a types.ContainerJSON with details about the container.
func (d *Docker) InspectContainer(containerID string) (types.ContainerJSON, error) {
	return d.Client.ContainerInspect(d.ctx, containerID)
}

// ContainerPortMapping returns a *nat.PortMap of all of the port mappings. This
// is basically just a convenience function that calls InspectContainer and
// roots through the return value for the port mapping.
func (d *Docker) ContainerPortMapping(containerID string) (nat.PortMap, error) {
	inspection, err := d.InspectContainer(containerID)
	if err != nil {
		return nil, err
	}
	return inspection.NetworkSettings.Ports, err
}

// RunStep will run the steps in a job. If a step fails, the function will
// return with a non-zero exit code. If an error occurs, the function will
// return with a non-zero exit code and a non-nil error.
func (d *Docker) RunStep(step *model.Step, invID string, idx int) (int64, error) {
	var (
		err             error
		wd, containerID string
	)

	stepIdx := strconv.Itoa(idx)

	if containerID, err = d.CreateContainerFromStep(step, invID); err != nil {
		return -1, err
	}

	wd, err = os.Getwd()
	if err != nil {
		return -1, err
	}
	stdoutpath := path.Join(wd, VOLUMEDIR, step.Stdout(stepIdx))
	logcabin.Info.Printf("path to the step stdout log file: %s\n", stdoutpath)
	stdoutFile, err := os.Create(stdoutpath)
	if err != nil {
		return -1, err
	}
	defer stdoutFile.Close()

	stderrpath := path.Join(wd, VOLUMEDIR, step.Stderr(stepIdx))
	logcabin.Info.Printf("path to the step stderr log file: %s\n", stderrpath)
	stderrFile, err := os.Create(stderrpath)
	if err != nil {
		return -1, err
	}
	defer stderrFile.Close()

	return d.runContainer(containerID, stdoutFile, stderrFile)
}

// PorkPull will pull the porklock image.
func (d *Docker) PorkPull() error {
	image := d.cfg.GetString("porklock.image")

	tag := d.cfg.GetString("porklock.tag")

	return d.Pull(image, tag)
}

// CreateDownloadContainer creates a container that can be used to download
// input files.
func (d *Docker) CreateDownloadContainer(job *model.Job, input *model.StepInput, idx string) (string, error) {
	var (
		wd, name, image, tag string
		response             container.ContainerCreateCreatedBody
		err                  error
	)

	config := &container.Config{}
	hostConfig := &container.HostConfig{}
	invID := job.InvocationID

	image = d.cfg.GetString("porklock.image")
	tag = d.cfg.GetString("porklock.tag")

	if err = d.PorkPull(); err != nil {
		return "", err
	}

	config.Image = fmt.Sprintf("%s:%s", image, tag)
	hostConfig.LogConfig = container.LogConfig{Type: "none"}

	config.WorkingDir = WORKDIR

	// make sure the host working dir is mounted and make it the default
	// working dir inside the container.
	if wd, err = os.Getwd(); err != nil {
		return "", err
	}

	// Check to see if a working directory volume exists
	hasVolume, err := d.VolumeExists(invID)
	if err != nil {
		return "", err
	}

	// if the working directory volume exists, use it.
	if hasVolume {
		hostConfig.Binds = append(
			hostConfig.Binds,
			fmt.Sprintf("%s:%s:%s", invID, WORKDIR, "rw"),
		)
	} else {
		hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s:%s", wd, WORKDIR, "rw"))
	}

	hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s:%s", wd, CONFIGDIR, "rw"))

	config.Labels = make(map[string]string)
	config.Labels[model.DockerLabelKey] = invID
	config.Labels[TypeLabel] = strconv.Itoa(InputContainer)
	config.Cmd = input.Arguments(job.Submitter, job.FileMetadata)

	logcabin.Info.Printf("hostconfig: %#v\n", hostConfig)
	logcabin.Info.Printf("config: %#v\n", config)

	name = fmt.Sprintf("input-%s-%s", idx, invID)
	if response, err = d.Client.ContainerCreate(d.ctx, config, hostConfig, nil, name); err == nil {
		logcabin.Info.Printf("created container %s", response.ID)
		for _, warning := range response.Warnings {
			logcabin.Info.Printf("Warning creating %s: %s", response.ID, warning)
		}
	}
	if err != nil {
		logcabin.Error.Print(err)
	}

	return response.ID, err
}

// DownloadInputs will run the docker containers that down input files into
// the local working directory.
func (d *Docker) DownloadInputs(job *model.Job, input *model.StepInput, idx int) (int64, error) {
	var (
		err                    error
		wd, containerID        string
		stdoutFile, stderrFile io.WriteCloser
	)

	inputIdx := strconv.Itoa(idx)

	if containerID, err = d.CreateDownloadContainer(job, input, inputIdx); err != nil {
		return -1, err
	}

	if wd, err = os.Getwd(); err != nil {
		return -1, err
	}

	stdoutpath := path.Join(wd, VOLUMEDIR, input.Stdout(inputIdx))
	logcabin.Info.Printf("creating stdout input log at %s\n", stdoutpath)
	if stdoutFile, err = os.Create(stdoutpath); err != nil {
		return -1, err
	}
	defer stdoutFile.Close()

	stderrpath := path.Join(wd, VOLUMEDIR, input.Stderr(inputIdx))
	logcabin.Info.Printf("creating stderr input log at %s\n", stderrpath)
	if stderrFile, err = os.Create(stderrpath); err != nil {
		return -1, err
	}
	defer stderrFile.Close()

	return d.runContainer(containerID, stdoutFile, stderrFile)
}

// CreateUploadContainer will initialize a container that will be used to
// upload job outputs into a directory in iRODS.
func (d *Docker) CreateUploadContainer(job *model.Job) (string, error) {
	var (
		err                  error
		image, tag, name, wd string
		response             container.ContainerCreateCreatedBody
	)

	config := &container.Config{}
	hostConfig := &container.HostConfig{}
	invID := job.InvocationID

	image = d.cfg.GetString("porklock.image")
	tag = d.cfg.GetString("porklock.tag")

	if err = d.PorkPull(); err != nil {
		return "", err
	}

	config.Image = fmt.Sprintf("%s:%s", image, tag)
	hostConfig.LogConfig = container.LogConfig{Type: "none"}

	config.WorkingDir = WORKDIR

	if wd, err = os.Getwd(); err != nil {
		return "", err
	}

	// Check to see if a working directory volume exists
	hasVolume, err := d.VolumeExists(invID)
	if err != nil {
		return "", err
	}

	// if the working directory volume exists, use it.
	if hasVolume {
		hostConfig.Binds = append(
			hostConfig.Binds,
			fmt.Sprintf("%s:%s:%s", invID, WORKDIR, "rw"),
		)
	} else {
		hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s:%s", wd, WORKDIR, "rw"))
	}

	hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s:%s", wd, CONFIGDIR, "rw"))

	config.Labels = make(map[string]string)
	config.Labels[model.DockerLabelKey] = job.InvocationID
	config.Labels[TypeLabel] = strconv.Itoa(OutputContainer)

	config.Cmd = job.FinalOutputArguments()

	logcabin.Info.Printf("hostconfig: %#v\n", hostConfig)
	logcabin.Info.Printf("config: %#v\n", config)

	name = fmt.Sprintf("output-%s", job.InvocationID)
	if response, err = d.Client.ContainerCreate(d.ctx, config, hostConfig, nil, name); err == nil {
		logcabin.Info.Printf("created container %s", response.ID)
		for _, warning := range response.Warnings {
			logcabin.Info.Printf("Warning creating %s: %s", response.ID, warning)
		}
	}
	if err != nil {
		logcabin.Error.Print(err)
	}

	return response.ID, err
}

// UploadOutputs will upload files to iRODS from the local working directory.
func (d *Docker) UploadOutputs(job *model.Job) (int64, error) {
	var (
		err                    error
		wd, containerID        string
		stdoutFile, stderrFile io.WriteCloser
	)
	if containerID, err = d.CreateUploadContainer(job); err != nil {
		return -1, err
	}

	if wd, err = os.Getwd(); err != nil {
		return -1, err
	}

	stdoutpath := path.Join(wd, VOLUMEDIR, "logs", "logs-stdout-output")
	logcabin.Info.Printf("path to the output stdout file: %s\n", stdoutpath)
	if stdoutFile, err = os.Create(stdoutpath); err != nil {
		return -1, err
	}
	defer stdoutFile.Close()

	stderrpath := path.Join(wd, VOLUMEDIR, "logs", "logs-stderr-output")
	logcabin.Info.Printf("path to the output stderr file: %s\n", stderrpath)
	if stderrFile, err = os.Create(stderrpath); err != nil {
		return -1, err
	}
	defer stderrFile.Close()

	return d.runContainer(containerID, stdoutFile, stderrFile)
}

// CreateDataContainer will create a data container that is required for the job.
func (d *Docker) CreateDataContainer(vf *model.VolumesFrom, invID string) (string, error) {
	var (
		err      error
		rw, name string
		response container.ContainerCreateCreatedBody
	)

	config := &container.Config{}
	hostConfig := &container.HostConfig{}

	config.Image = fmt.Sprintf("%s:%s", vf.Name, vf.Tag)
	hostConfig.LogConfig = container.LogConfig{Type: "none"}

	config.Labels = make(map[string]string)
	config.Labels[model.DockerLabelKey] = invID
	config.Labels[TypeLabel] = strconv.Itoa(DataContainer)

	if vf.HostPath != "" || vf.ContainerPath != "" {
		if vf.ReadOnly {
			rw = "ro"
		} else {
			rw = "rw"
		}
		hostConfig.Binds = append(
			hostConfig.Binds,
			fmt.Sprintf("%s:%s:%s", vf.HostPath, vf.ContainerPath, rw),
		)
	}

	config.Cmd = []string{"/bin/true"}
	name = fmt.Sprintf("%s-%s", vf.NamePrefix, invID)
	if response, err = d.Client.ContainerCreate(d.ctx, config, hostConfig, nil, name); err == nil {
		logcabin.Info.Printf("created container %s", response.ID)
		for _, warning := range response.Warnings {
			logcabin.Info.Printf("Warning creating %s: %s", response.ID, warning)
		}
	}

	return response.ID, nil
}
