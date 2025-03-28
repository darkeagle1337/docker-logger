package discovery

import (
	"regexp"
	"strings"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	log "github.com/go-pkgz/lgr"
	"github.com/pkg/errors"
)

// EventNotif emits all changes from all containers states
type EventNotif struct {
	dockerClient   DockerClient
	excludes       []string
	includes       []string
	includesRegexp *regexp.Regexp
	excludesRegexp *regexp.Regexp
	eventsCh       chan Event
}

// Event is simplified docker.APIEvents for containers only, exposed to caller
type Event struct {
	ContainerID   string
	ContainerName string
	Group         string // group is the "path" part of the image tag, i.e. for umputun/system/logger:latest it will be "system"
	TS            time.Time
	Status        bool
}

// DockerClient defines interface listing containers and subscribing to events
type DockerClient interface {
	ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error)
	AddEventListener(listener chan<- *docker.APIEvents) error
}

var reGroup = regexp.MustCompile(`/(.*?)/`)
var reSwarm = regexp.MustCompile(`(?m)(.*)\.(\d+)\.(.*)`)

// NewEventNotif makes EventNotif publishing all changes to eventsCh
func NewEventNotif(dockerClient DockerClient, excludes, includes []string, includesPattern, excludesPattern string) (*EventNotif, error) {
	log.Printf("[DEBUG] create events notif, excludes: %+v, includes: %+v, includesPattern: %+v, excludesPattern: %+v",
		excludes, includes, includesPattern, excludesPattern)

	var err error
	var includesRe *regexp.Regexp
	if includesPattern != "" {
		includesRe, err = regexp.Compile(includesPattern)
		if err != nil {
			return nil, errors.Wrap(err, "failed to compile includesPattern")
		}
	}

	var excludesRe *regexp.Regexp
	if excludesPattern != "" {
		excludesRe, err = regexp.Compile(excludesPattern)
		if err != nil {
			return nil, errors.Wrap(err, "failed to compile excludesPattern")
		}
	}

	res := EventNotif{
		dockerClient:   dockerClient,
		excludes:       excludes,
		includes:       includes,
		includesRegexp: includesRe,
		excludesRegexp: excludesRe,
		eventsCh:       make(chan Event, 100),
	}

	// first get all currently running containers
	if err := res.emitRunningContainers(); err != nil {
		return nil, errors.Wrap(err, "failed to emit containers")
	}

	go func() {
		res.activate(dockerClient) // activate listener for new container events
	}()

	return &res, nil
}

// Channel gets eventsCh with all containers events
func (e *EventNotif) Channel() (res <-chan Event) {
	return e.eventsCh
}

// activate starts blocking listener for all docker events
// filters everything except "container" type, detects stop/start events and publishes to eventsCh
func (e *EventNotif) activate(client DockerClient) {
	dockerEventsCh := make(chan *docker.APIEvents)
	if err := client.AddEventListener(dockerEventsCh); err != nil {
		log.Fatalf("[ERROR] can't add even listener, %v", err)
	}

	upStatuses := []string{"start", "restart"}
	downStatuses := []string{"die", "destroy", "stop", "pause"}

	for dockerEvent := range dockerEventsCh {
		if dockerEvent.Type != "container" {
			continue
		}

		if !contains(dockerEvent.Status, upStatuses) && !contains(dockerEvent.Status, downStatuses) {
			continue
		}

		log.Printf("[DEBUG] api event %+v", dockerEvent)
		containerName := buildContainerName(dockerEvent.Actor.Attributes, strings.TrimPrefix(dockerEvent.Actor.Attributes["name"], "/"))
		groupName := buildGroupName(dockerEvent.Actor.Attributes, e.group(dockerEvent.From))
		if !e.isAllowed(containerName) {
			log.Printf("[INFO] container %s excluded", containerName)
			continue
		}

		event := Event{
			ContainerID:   dockerEvent.Actor.ID,
			ContainerName: containerName,
			Status:        contains(dockerEvent.Status, upStatuses),
			TS:            time.Unix(dockerEvent.Time/1000, dockerEvent.TimeNano),
			Group:         groupName,
		}
		log.Printf("[INFO] new event %+v", event)
		e.eventsCh <- event
	}
	log.Fatalf("[ERROR] event listener failed")
}

// emitRunningContainers gets all currently running containers and publishes them as "Status=true" (started) events
func (e *EventNotif) emitRunningContainers() error {
	containers, err := e.dockerClient.ListContainers(docker.ListContainersOptions{All: false})
	if err != nil {
		return errors.Wrap(err, "can't list containers")
	}
	log.Printf("[DEBUG] total containers = %d", len(containers))

	for _, c := range containers {
		containerName := buildContainerName(c.Labels, strings.TrimPrefix(c.Names[0], "/"))
		groupName := buildGroupName(c.Labels, e.group(c.Image))
		if !e.isAllowed(containerName) {
			log.Printf("[INFO] container %s excluded", containerName)
			continue
		}
		event := Event{
			Status:        true,
			ContainerName: containerName,
			ContainerID:   c.ID,
			TS:            time.Unix(c.Created/1000, 0),
			Group:         groupName,
		}
		log.Printf("[DEBUG] running container added, %+v", event)
		e.eventsCh <- event
	}
	log.Print("[DEBUG] completed initial emit")
	return nil
}

func (e *EventNotif) group(image string) string {
	if r := reGroup.FindStringSubmatch(image); len(r) == 2 {
		return r[1]
	}
	log.Printf("[DEBUG] no group for %s", image)
	return ""
}

func (e *EventNotif) isAllowed(containerName string) bool {
	if e.includesRegexp != nil {
		return e.includesRegexp.MatchString(containerName)
	}
	if e.excludesRegexp != nil {
		return !e.excludesRegexp.MatchString(containerName)
	}
	if len(e.includes) > 0 {
		return contains(containerName, e.includes)
	}
	if contains(containerName, e.excludes) {
		return false
	}

	return true
}

func contains(e string, s []string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func buildContainerName(labels map[string]string, containerName string) string {
	result := []string{}
	if r := reSwarm.FindStringSubmatch(containerName); len(r) == 4 {
		result = append(result, r[1]) // service name
		result = append(result, r[2]) // replica number
	} else if labelName, ok := labels["logger.container.name"]; ok && labelName != "" {
		result = append(result, labelName)
	}
	if len(result) > 0 {
		return strings.Join(result, "-")
	}
	return containerName
}

func buildGroupName(labels map[string]string, defaultValue string) string {
	if labelGroup, ok := labels["logger.group.name"]; ok && labelGroup != "" {
		return labelGroup
	}

	return defaultValue
}
