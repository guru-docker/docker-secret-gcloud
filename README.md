# Docker secret provider for Google Cloud Secret Manager

This plugin lets Docker Swarm read secret values straight out of [Google Cloud
Secret Manager](https://cloud.google.com/secret-manager) instead of storing them
in the swarm's raft log.

[![CI](https://github.com/guru-docker/docker-secret-gcloud/actions/workflows/ci.yml/badge.svg)](https://github.com/guru-docker/docker-secret-gcloud/actions/workflows/ci.yml)

Secrets created with a driver hold no data of their own: the swarm keeps only
the name and labels, and asks the plugin for the value each time a task that
needs it is scheduled. Rotating a version in Secret Manager therefore reaches
containers on their next start, with nothing to update in Docker.

Secret drivers are a swarm feature, so the plugin must be installed on every
manager. `docker run --secret` on a standalone engine does not use it.

If what you want is to ship your own ciphertext and have it unwrapped with a KMS
key instead of reading a hosted store, use
[docker-config-gcloud](https://github.com/guru-docker/docker-config-gcloud).

## Usage

0 - Create the secret in Secret Manager, once

```
$ printf 'the-value' | gcloud secrets create api-key \
    --replication-policy=automatic --data-file=- --project=<project>
```

> This plugin never creates anything. It calls one API, `AccessSecretVersion`,
> so its service account needs only `roles/secretmanager.secretAccessor` on the
> secret -- not `secrets.create`. A `docker secret create` naming a secret that
> does not exist here succeeds, because Docker does not consult the driver at
> that point; the failure surfaces later, when the first task that mounts it is
> scheduled and the driver answers `NotFound`.

1 - Install the plugin on each swarm manager

```
$ docker plugin install glabservices/gcloud-secret \
    GOOGLE_CLOUD_PROJECT=<project>

# or to enable debug
$ docker plugin install glabservices/gcloud-secret DEBUG=1

# or to point at a host directory holding a credentials file
$ docker plugin install glabservices/gcloud-secret \
    gcloud.source=<any_folder>
```

2 - Create the swarm secret that points at it

```
$ docker secret create \
    --driver glabservices/gcloud-secret \
    -l gcloud.secret=api-key \
    api-key
qkw4l4kjqjqkl4jkl4jqkl4jq

$ docker secret ls
ID                          NAME      DRIVER
qkw4l4kjqjqkl4jkl4jqkl4jq   api-key   glabservices/gcloud-secret
```

3 - Use the secret

```
$ docker service create --name app --secret api-key myimage
```

The value lands at `/run/secrets/api-key` in the task, exactly as a normal
swarm secret does.

## Creating secrets through the plugin

`docker secret create` cannot provision anything in Secret Manager. Docker writes
the name and labels into the swarm's raft store and never consults the driver, so
a swarm secret naming a secret that does not exist succeeds, and only fails later
when a task tries to mount it.

The plugin therefore exposes a route of its own, `/SecretProvider.CreateSecret`,
on the same socket. Docker never calls it; `scripts/gcloud-secret-ctl` does:

```
# a value nobody needs to choose -- a password, a token, a signing key
$ gcloud-secret-ctl create api-key --generate 32

# a value issued elsewhere
$ gcloud-secret-ctl create stripe-key --value-file ./stripe.txt
$ printf '%s' "$KEY" | gcloud-secret-ctl create stripe-key --value-file -

# add a version to a secret that already exists, i.e. rotate it
$ gcloud-secret-ctl create api-key --generate 32 --if-missing

# read a value back
$ gcloud-secret-ctl get api-key
```

The point is where the credentials live. The write happens inside the plugin,
using the service account it already holds, so provisioning needs no GCP
credentials on your workstation -- only root on a node where the plugin runs,
since the plugin socket is `root`-owned.

Two consequences worth understanding before enabling this:

- The service account needs `secretmanager.secrets.create` and
  `secretmanager.versions.add` in addition to `secretAccessor`. That is a wider
  grant on every manager. If provisioning is rare, doing it with `gcloud` under
  your own credentials keeps the managers read-only, which is the safer default.
- Creating an existing secret is an error unless `--if-missing` is given, so a
  typo cannot quietly add a version to the wrong secret.

The value never enters the swarm's raft store: it travels over the plugin socket
straight to Secret Manager. A generated value is not echoed back either -- read
it with `gcloud-secret-ctl get` if something needs it.

## Config files

A Secret Manager payload is bytes, up to 64KiB, so a whole config file goes in
exactly as an API key does. Paired with an absolute `target`, this plugin puts
it wherever the service expects to find it:

```
$ gcloud secrets create app-config --replication-policy=automatic \
    --data-file=./app.yaml

$ docker secret create -d glabservices/gcloud-secret \
    -l gcloud.secret=app-config \
    app-config

$ docker service create --name app \
    --secret source=app-config,target=/etc/app/config.yaml,mode=0444 \
    myimage
```

The file lands at `/etc/app/config.yaml` with the mode you asked for, and
`/run/secrets` is not involved. Nothing about the value being configuration
rather than a credential changes how it is fetched, so rotation works the same
way: add a version in Secret Manager and the next task to start picks it up.

Use a plain `docker config` when the contents are not sensitive. It costs no API
call when a task starts, does not depend on Secret Manager being reachable, and
`docker config inspect` shows you the value while you are debugging. Reach for
this plugin when that last property is the problem -- a driver-backed secret
keeps nothing in the swarm's raft log but the name and labels, and the value
cannot be read back at all.

## Options

The swarm secret carries labels that tell the plugin which version to read.
With no labels at all it reads the *latest* version of a secret whose id is the
docker secret's own name, in the plugin's default project.

| Label                  | Required | Description                                                             |
| ---------------------- | -------- | ----------------------------------------------------------------------- |
| `gcloud.project`       | no       | Project holding the secret. Defaults to `GOOGLE_CLOUD_PROJECT`.         |
| `gcloud.secret`        | no       | Secret id. Defaults to the docker secret's name.                        |
| `gcloud.version`       | no       | Version to read. Defaults to `latest`.                                  |
| `gcloud.resource`      | no       | Full resource name; overrides the three labels above.                   |
| `gcloud.do_not_reuse`  | no       | `true` makes the swarm re-read the value for every task.                |

```
# a pinned version in another project
$ docker secret create -d glabservices/gcloud-secret \
    -l gcloud.project=acme-prod \
    -l gcloud.secret=api-key \
    -l gcloud.version=7 \
    api-key

# the same thing as one resource name, regional secrets included
$ docker secret create -d glabservices/gcloud-secret \
    -l gcloud.resource=projects/acme-prod/locations/europe-west1/secrets/api-key/versions/7 \
    api-key
```

Docker caches a driver's answer and reuses it for further tasks of the same
secret. `-l gcloud.do_not_reuse=true` turns that off, so a task scheduled after
a rotation picks up the new version of a `latest` secret immediately.

## Settings

Plugin variables are set at install time, or with `docker plugin set` while the
plugin is disabled.

| Variable                         | Default | Description                                              |
| -------------------------------- | ------- | -------------------------------------------------------- |
| `GOOGLE_CLOUD_PROJECT`           |         | Project used when a secret carries no `gcloud.project`.  |
| `GOOGLE_APPLICATION_CREDENTIALS` |         | Path *inside the plugin* to a credentials file.          |
| `GCLOUD_TIMEOUT`                 | `30s`   | Deadline for a single Secret Manager call.               |
| `GCLOUD_REQUIRE_CREDENTIALS_FILE`| `0`     | `1` refuses to start without a credentials file.         |
| `DEBUG`                          | `0`     | `1` turns on debug logging.                              |

## Authentication

The plugin uses Application Default Credentials, and resolves them in this
order:

1. `GOOGLE_APPLICATION_CREDENTIALS`, if set, as a path inside the plugin's
   filesystem — the `gcloud` mount below is how a host file gets there.
2. `/run/gcloud/credentials.json`, if it exists. This is the zero-configuration
   path: drop a key in the mounted directory and nothing else needs setting.
3. The GCE/GKE metadata server, on a Google Cloud host. Nothing to mount at all;
   grant the node's service account `secretAccessor` and you are done.

The credentials file may be either a service account key or a workload identity
federation (`external_account`) config, which is the keyless option for managers
running outside Google Cloud.

If the credentials live on a remote filesystem, set
`GCLOUD_REQUIRE_CREDENTIALS_FILE=1`. Docker binds the host directory at the
moment the plugin is enabled, so a filesystem mounted *after* that point never
becomes visible to the plugin — it keeps reading the empty directory underneath
while the file is plainly there on the host. With the requirement set, the
plugin refuses to start rather than falling through to the metadata server, and
`docker plugin enable` fails with the reason in the daemon log. Order dockerd
after the mount to avoid the situation in the first place.

The plugin bind-mounts a host directory at `/run/gcloud`, read only. It defaults
to Docker's own plugin directory, which holds no credentials — so out of the box
step 2 finds nothing and the plugin falls through to the metadata server. Point
it at wherever the key lives:

```
$ docker plugin install glabservices/gcloud-secret \
    gcloud.source=/etc/gcloud

# on an already installed plugin
$ docker plugin disable glabservices/gcloud-secret
$ docker plugin set glabservices/gcloud-secret gcloud.source=/etc/gcloud
$ docker plugin enable glabservices/gcloud-secret
```

## Integrity

Secret Manager returns a CRC32C checksum alongside the payload. The plugin
verifies it and fails the request rather than handing a container bytes that
were damaged in transit.

## Development

```
# unit tests and static checks
$ ./scripts/unit.sh

# build the managed plugin locally
$ make

# end-to-end tests (needs docker, swarm, plugin install rights and curl)
$ sudo ./scripts/integration.sh
```

The integration suite runs its uncredentialed cases anywhere; set
`GOOGLE_CLOUD_PROJECT` and `GCLOUD_SECRET` to also exercise a real secret.

`make` targets the local Docker engine by default. Override it with
`make DOCKER="docker --context=<name>"` to build against another engine, and
`PLUGIN_NAME` / `PLUGIN_TAG` to change what is built.

## LICENSE

MIT
