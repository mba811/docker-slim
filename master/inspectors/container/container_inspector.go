package container

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudimmunity/docker-slim/master/config"
	"github.com/cloudimmunity/docker-slim/master/docker/dockerhost"
	"github.com/cloudimmunity/docker-slim/master/inspectors/container/ipc"
	"github.com/cloudimmunity/docker-slim/master/inspectors/image"
	"github.com/cloudimmunity/docker-slim/master/security/apparmor"
	"github.com/cloudimmunity/docker-slim/master/security/seccomp"
	"github.com/cloudimmunity/docker-slim/messages"
	"github.com/cloudimmunity/docker-slim/utils"

	log "github.com/Sirupsen/logrus"
	dockerapi "github.com/cloudimmunity/go-dockerclientx"
)

type Inspector struct {
	ContainerInfo     *dockerapi.Container
	ContainerID       string
	FatContainerCmd   []string
	LocalVolumePath   string
	CmdPort           dockerapi.Port
	EvtPort           dockerapi.Port
	DockerHostIP      string
	ImageInspector    *image.Inspector
	ApiClient         *dockerapi.Client
	Overrides         *config.ContainerOverrides
	ShowContainerLogs bool
	VolumeMounts      map[string]config.VolumeMount
	ExcludePaths      map[string]bool
	IncludePaths      map[string]bool
	DoDebug           bool
}

func pathMapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}

func NewInspector(client *dockerapi.Client,
	imageInspector *image.Inspector,
	localVolumePath string,
	overrides *config.ContainerOverrides,
	showContainerLogs bool,
	volumeMounts map[string]config.VolumeMount,
	excludePaths map[string]bool,
	includePaths map[string]bool,
	doDebug bool) (*Inspector, error) {

	inspector := &Inspector{
		LocalVolumePath:   localVolumePath,
		CmdPort:           "65501/tcp",
		EvtPort:           "65502/tcp",
		ImageInspector:    imageInspector,
		ApiClient:         client,
		Overrides:         overrides,
		ShowContainerLogs: showContainerLogs,
		VolumeMounts:      volumeMounts,
		ExcludePaths:      excludePaths,
		IncludePaths:      includePaths,
		DoDebug:           doDebug,
	}

	if overrides != nil && ((len(overrides.Entrypoint) > 0) || overrides.ClearEntrypoint) {
		log.Debugf("overriding Entrypoint %+v => %+v (%v)\n",
			imageInspector.ImageInfo.Config.Entrypoint, overrides.Entrypoint, overrides.ClearEntrypoint)
		if len(overrides.Entrypoint) > 0 {
			inspector.FatContainerCmd = append(inspector.FatContainerCmd, overrides.Entrypoint...)
		}

	} else if len(imageInspector.ImageInfo.Config.Entrypoint) > 0 {
		inspector.FatContainerCmd = append(inspector.FatContainerCmd, imageInspector.ImageInfo.Config.Entrypoint...)
	}

	if overrides != nil && ((len(overrides.Cmd) > 0) || overrides.ClearCmd) {
		log.Debugf("overriding Cmd %+v => %+v (%v)\n",
			imageInspector.ImageInfo.Config.Cmd, overrides.Cmd, overrides.ClearCmd)
		if len(overrides.Cmd) > 0 {
			inspector.FatContainerCmd = append(inspector.FatContainerCmd, overrides.Cmd...)
		}

	} else if len(imageInspector.ImageInfo.Config.Cmd) > 0 {
		inspector.FatContainerCmd = append(inspector.FatContainerCmd, imageInspector.ImageInfo.Config.Cmd...)
	}

	return inspector, nil
}

func (i *Inspector) RunContainer() error {
	artifactsPath := filepath.Join(i.LocalVolumePath, "artifacts")
	sensorPath := filepath.Join(utils.ExeDir(), "docker-slim-sensor")

	artifactsMountInfo := fmt.Sprintf("%s:/opt/dockerslim/artifacts", artifactsPath)
	sensorMountInfo := fmt.Sprintf("%s:/opt/dockerslim/bin/sensor:ro", sensorPath)

	var volumeBinds []string
	for _, volumeMount := range i.VolumeMounts {
		mountInfo := fmt.Sprintf("%s:%s:%s", volumeMount.Source, volumeMount.Destination, volumeMount.Options)
		volumeBinds = append(volumeBinds, mountInfo)
	}

	volumeBinds = append(volumeBinds, artifactsMountInfo)
	volumeBinds = append(volumeBinds, sensorMountInfo)

	var containerCmd []string
	if i.DoDebug {
		containerCmd = append(containerCmd, "-d")
	}

	containerOptions := dockerapi.CreateContainerOptions{
		Name: "dockerslimk",
		Config: &dockerapi.Config{
			Image: i.ImageInspector.ImageRef,
			ExposedPorts: map[dockerapi.Port]struct{}{
				i.CmdPort: struct{}{},
				i.EvtPort: struct{}{},
			},
			Entrypoint: []string{"/opt/dockerslim/bin/sensor"},
			Cmd:        containerCmd,
			Labels:     map[string]string{"type": "dockerslim"},
		},
		HostConfig: &dockerapi.HostConfig{
			Binds:           volumeBinds,
			PublishAllPorts: true,
			CapAdd:          []string{"SYS_ADMIN"},
			Privileged:      true,
		},
	}

	containerInfo, err := i.ApiClient.CreateContainer(containerOptions)
	if err != nil {
		return err
	}

	i.ContainerID = containerInfo.ID
	log.Infoln("docker-slim: created container =>", i.ContainerID)

	if err := i.ApiClient.StartContainer(i.ContainerID, &dockerapi.HostConfig{
		PublishAllPorts: true,
		CapAdd:          []string{"SYS_ADMIN"},
		Privileged:      true,
	}); err != nil {
		return err
	}

	if i.ContainerInfo, err = i.ApiClient.InspectContainer(i.ContainerID); err != nil {
		return err
	}

	utils.FailWhen(i.ContainerInfo.NetworkSettings == nil, "docker-slim: error => no network info")
	log.Debugf("container NetworkSettings.Ports => %#v\n", i.ContainerInfo.NetworkSettings.Ports)

	if err = i.initContainerChannels(); err != nil {
		return err
	}

	cmd := &messages.StartMonitor{
		AppName: i.FatContainerCmd[0],
	}

	if len(i.FatContainerCmd) > 1 {
		cmd.AppArgs = i.FatContainerCmd[1:]
	}

	if len(i.ExcludePaths) > 0 {
		cmd.Excludes = pathMapKeys(i.ExcludePaths)
	}

	if len(i.IncludePaths) > 0 {
		cmd.Includes = pathMapKeys(i.IncludePaths)
	}

	_, err = ipc.SendContainerCmd(cmd)
	return err
}

func (i *Inspector) ShutdownContainer() error {
	i.shutdownContainerChannels()

	if i.ShowContainerLogs {
		var outData bytes.Buffer
		outw := bufio.NewWriter(&outData)
		var errData bytes.Buffer
		errw := bufio.NewWriter(&errData)

		log.Debug("docker-slim: getting container logs => ", i.ContainerID)
		logsOptions := dockerapi.LogsOptions{
			Container:    i.ContainerID,
			OutputStream: outw,
			ErrorStream:  errw,
			Stdout:       true,
			Stderr:       true,
		}

		err := i.ApiClient.Logs(logsOptions)
		if err != nil {
			log.Infof("docker-slim: error getting container logs => %v - %v\n", i.ContainerID, err)
		} else {
			outw.Flush()
			errw.Flush()
			fmt.Println("docker-slim: container stdout:")
			outData.WriteTo(os.Stdout)
			fmt.Println("docker-slim: container stderr:")
			errData.WriteTo(os.Stdout)
		}
	}

	err := i.ApiClient.StopContainer(i.ContainerID, 9)
	utils.WarnOn(err)

	removeOption := dockerapi.RemoveContainerOptions{
		ID:            i.ContainerID,
		RemoveVolumes: true,
		Force:         true,
	}
	err = i.ApiClient.RemoveContainer(removeOption)
	return nil
}

func (i *Inspector) FinishMonitoring() {
	cmdResponse, err := ipc.SendContainerCmd(&messages.StopMonitor{})
	utils.WarnOn(err)
	_ = cmdResponse

	log.Debugf("'stop' response => '%v'\n", cmdResponse)
	log.Info("docker-slim: waiting for the container finish its work...")

	//for now there's only one event ("done")
	//getEvt() should timeout in two minutes (todo: pick a good timeout)
	evt, err := ipc.GetContainerEvt()
	utils.WarnOn(err)
	_ = evt
	log.Debugf("docker-slim: sensor event => '%v'\n", evt)
}

func (i *Inspector) initContainerChannels() error {
	/*
		NOTE: not using IPC for now... (future option for regular Docker deployments)
		ipcLocation := filepath.Join(localVolumePath,"ipc")
		_, err = os.Stat(ipcLocation)
		if os.IsNotExist(err) {
			os.MkdirAll(ipcLocation, 0777)
			_, err = os.Stat(ipcLocation)
			utils.FailOn(err)
		}
	*/

	cmdPortBindings := i.ContainerInfo.NetworkSettings.Ports[i.CmdPort]
	evtPortBindings := i.ContainerInfo.NetworkSettings.Ports[i.EvtPort]
	i.DockerHostIP = dockerhost.GetIP()

	if err := ipc.InitContainerChannels(i.DockerHostIP, cmdPortBindings[0].HostPort, evtPortBindings[0].HostPort); err != nil {
		return err
	}

	return nil
}

func (i *Inspector) shutdownContainerChannels() {
	ipc.ShutdownContainerChannels()
}

func (i *Inspector) ProcessCollectedData() error {
	log.Info("docker-slim: generating AppArmor profile...")
	err := apparmor.GenProfile(i.ImageInspector.ArtifactLocation, i.ImageInspector.AppArmorProfileName)
	if err != nil {
		return err
	}

	return seccomp.GenProfile(i.ImageInspector.ArtifactLocation, i.ImageInspector.SeccompProfileName)
}
