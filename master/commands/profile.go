package commands

import (
	"bufio"
	"fmt"
	"os"

	"github.com/cloudimmunity/docker-slim/master/config"
	"github.com/cloudimmunity/docker-slim/master/inspectors/container"
	"github.com/cloudimmunity/docker-slim/master/inspectors/container/probes/http"
	"github.com/cloudimmunity/docker-slim/master/inspectors/image"
	"github.com/cloudimmunity/docker-slim/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/cloudimmunity/go-dockerclientx"
	"github.com/dustin/go-humanize"
)

func OnProfile(doDebug bool,
	imageRef string,
	doHttpProbe bool,
	doShowContainerLogs bool,
	overrides *config.ContainerOverrides,
	volumeMounts map[string]config.VolumeMount,
	excludePaths map[string]bool,
	includePaths map[string]bool) {
	fmt.Printf("docker-slim: [profile] image=%v\n", imageRef)
	doRmFileArtifacts := false

	client, _ := docker.NewClientFromEnv()

	imageInspector, err := image.NewInspector(client, imageRef)
	utils.FailOn(err)

	log.Info("docker-slim: inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	utils.FailOn(err)

	localVolumePath, artifactLocation := utils.PrepareSlimDirs(imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation

	log.Infof("docker-slim: [%v] 'fat' image size => %v (%v)\n",
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	log.Info("docker-slim: processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	utils.FailOn(err)

	containerInspector, err := container.NewInspector(client,
		imageInspector,
		localVolumePath,
		overrides,
		doShowContainerLogs,
		volumeMounts,
		excludePaths,
		includePaths,
		doDebug)
	utils.FailOn(err)

	log.Info("docker-slim: starting instrumented 'fat' container...")
	err = containerInspector.RunContainer()
	utils.FailOn(err)

	log.Info("docker-slim: watching container monitor...")

	if doHttpProbe {
		probe, err := http.NewRootProbe(containerInspector)
		utils.FailOn(err)
		probe.Start()
	}

	fmt.Println("docker-slim: press any key when you are done using the container...")
	creader := bufio.NewReader(os.Stdin)
	_, _, _ = creader.ReadLine()

	containerInspector.FinishMonitoring()

	log.Info("docker-slim: shutting down 'fat' container...")
	err = containerInspector.ShutdownContainer()
	utils.WarnOn(err)

	log.Info("docker-slim: processing instrumented 'fat' container info...")
	err = containerInspector.ProcessCollectedData()
	utils.FailOn(err)

	if doRmFileArtifacts {
		log.Info("docker-slim: removing temporary artifacts...")
		err = utils.RemoveArtifacts(artifactLocation) //TODO: remove only the "files" subdirectory
		utils.WarnOn(err)
	}

	fmt.Println("docker-slim: [profile] done.")
}
