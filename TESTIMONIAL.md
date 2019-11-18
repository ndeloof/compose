# Docker-compose project testimonial

## About

### Role
This document defines the goals and organisation of the docker-compose project. It establishes high-level role of the
project aside other Docker tools, which will offer guidance on decision making. It also defines the process we use to
manage project evolution and clarify expectations regarding community feedback and contributions.

docker-compose started as [fig](http://fig.sh), a company being acquired by Docker in 2015, and has since grown
organically. Actual usages are far from being limited to just running a multi-container application, and demonstrate
both creativity of our ecosystem and universality of the Docker Compose file format to define composition of containers
for many use-cases.

This document aims to give docker-compose well established objectives, so that it can evolve with a cleaner, long terms
methodology to handle community feedback and feature requests.

### Terminology
To avoid ambiguities, in this document we use “Docker Compose” for the yaml file format to define container based
application definitions, to be distinguished from “docker-compose” which is the command line tool hosted on
https://github.com/docker/compose.

## Objectives
docker-compose is a command line tool to:

- manage lifecycle for an application built as a composition of containers and Docker engine resources (volumes,
  network, …).
- Improve local developer productivity ("[inner-loop](https://en.wikipedia.org/wiki/Inner_loop)") with container
  replacement and resource reuse.
- Support debugging scenarios by entering containers or patching application definition with additional debugging
  resource definition

## Mission statement
Any decision made by maintainer should be guided by the project mission statements. Those are by design high-level and
non-technical, to ensure they can help us make the right decisions regarding specific issues and enforce homogeneity of
docker-compose.

### 1. Developer focus
While we acknowledge Docker Compose file format is used to define production deployment (by
[docker stack](https://docs.docker.com/engine/reference/commandline/stack/),
[compose-on-kubernetes](https://github.com/docker/compose-on-kubernetes) or
[Amason ECS](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/cmd-ecs-cli-compose.html)),
docker-compose itself is designed as a developer tool and will focus on specific needs of such a usage. docker-compose
does implement some specific options and usages to offer an efficient
[inner-loop](https://en.wikipedia.org/wiki/Inner_loop) which assumes a local, single-host Docker engine.

### 2. Docker CLI consistency
We will keep docker-compose consistent with the Docker CLI. docker-compose won’t implement any fix or feature that
should be implemented by the CLI, neither will it change the behaviour of any CLI option that is supported by the
Docker Compose file format, until it is required to implement a better developer user experience on local Docker
installation. A typical sample for this is support of relative paths for bind-mounts. Otherwise docker-compose should
always behave as the plain Docker CLI for equivalent features.

### 3. Avoid features duplication
As an extension to previous statement, we want to avoid code duplication with Docker CLI, and will delegate to CLI when
relevant. At time writing this already is a requirement in a few places, for example for `interactive` support. This
approach might be considered before trying to implement new features in docker-compose, as we will anyway have to
ensure a consistent behaviour in Docker CLI to respect statement #2.

### 4. Support Docker features
As a consequence of Statement #2, docker-compose should evolve in harmony with the Docker tools ecosystem. Which means
new features added to the Docker CLI should be supported in docker-compose, as long as they can be expressed in the
Docker Compose file format. The other way, other Docker CLI commands and CLI plugins to offer Docker Compose support
(docker stack, docker app, …) should be promoted by docker-compose when they are more relevant based on maintainers
criteria (efficiency, flexibility, robustness, maintainability, etc).

## Decision process
This section describes project management process from an open-source contributor point of view, Docker Inc internal
project management and allocated engineering resources is out of scope.

### Feature Requests
Feature requests can be reported by users on [project’s GitHub](https://github.com/docker/compose). Those will be
checked for validity and tagged accordingly during initial triage. They are then be discussed by maintainers during
maintainer meeting (to take place twice a month with flexible schedule). Maintainer do consider benefits and impacts
and decide if a feature should be added to backlog or rejected. Github issue is then updated to report maintainer
decision with reasoning on approval or rejection. Actual implementation of an approved feature request can’t be
guaranteed, and is open to contributions by pull-requests.

### Issues
Issue report are reviewed by initial triage to first confirm report completeness with enough details to define a
reproduction scenario or at least offer relevant investigation data. This initial triage also ensure issues don’t
conflict with docker-compose mission statement, typically that the reported issue doesn’t also apply to Docker CLI or
relevant tool involved. In such case it is immediately rejected. Maintainer do review issues having been sorted by
triage and add them to backlog for further investigation.

### Compatibility and breaking changes
The decision process might result in approving some breaking-changes. In such circumstances the maintainers will
acknowledge the break and bump docker-compose major version to avoid confusion.



## References

Project repository: https://github.com/docker/compose

Official documentation: https://docs.docker.com/compose/

Docker Compose file format specification: https://github.com/docker/compose/blob/master/compose/config/config_schema_v3.7.json
