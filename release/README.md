# Istio Release

- [Istio Release](#istio-release)
  * [Overview](#overview)
  * [Semi-automated release since 0.2](#semi-automated-release-since-02)
  * [Manual release process (DEPRECATED)](#manual-release-process-deprecated)
    + [Creating tags](#creating-tags)
    + [Rebuild artifacts to include the tags](#rebuild-artifacts-to-include-the-tags)
    + [Updating ```istio.VERSION```](#updating----istioversion---)
    + [Creating archives](#creating-archives)
    + [Finalizing the release](#finalizing-the-release)

## Overview

The release is started from the [istio/istio](https://github.com/istio/istio) module.

Istio release is currently composed of artifacts for the following repos:

* [auth](https://github.com/istio/auth)
* [pilot](https://github.com/istio/pilot)
* [mixer](https://github.com/istio/mixer)
* [proxy](https://github.com/istio/proxy)

The release consists in retagging the artifacts and creating new annotated tags.

Only organization members part of the [Release Engineers](https://github.com/orgs/istio/teams/release-engineers/members) team may create a release.

If you are making a release from a branch, use the branch name, e.g. `BRANCH=release-0.1` for 0.1 or `master` for master.

## Release Preparation

Before any release we need to make sure that all components are using the same
version of [istio/api](https://github.com/istio/api/commits/master).

As of today API is used in
* [pilot](https://github.com/istio/pilot/blob/master/WORKSPACE#L464), value of `ISTIO_API`.
* [mixer](https://github.com/istio/mixer/blob/master/istio_api.bzl#L18), value of `ISTIO_API_SHA`.
* [mixerclient](https://github.com/istio/mixerclient/blob/master/repositories.bzl#L335), value of `ISTIO_API`.

For mixerclient, it gets more complicated. We need to update proxy to use the
last version, and then update pilot a second time to use the last proxy.  Further,  mixerclient requires someone who has write access to the repo to manually merge the [istio/api](https://github.com/istio/api/commits/master) version update.

## Semi-automated release since 0.2

The release process is semi-automated starting with release 0.2.
It is still driven from a release engineer desktop but all actions are automated
using [githubctl](https://github.com/istio/test-infra/blob/master/toolbox/githubctl/main.go),
a tool of our own that acts as a GitHub client making REST calls through the GitHub API.
One may get githubctl from the istio/test-infra repository


You will need a ```<github token file>``` text file containing the github peronal access token setup following the [instruction](https://github.com/istio/istio/blob/master/devel/README.md#setting-up-a-personal-access-token)

```
$ git clone https://github.com/istio/test-infra.git
```

and build it using

```
$ bazel build //toolbox/githubctl
```

The binary output is located in bazel-bin/toolbox/githubctl/githubctl.

```
$ alias githubctl="${PWD}/bazel-bin/toolbox/githubctl/githubctl"
```

The release process goes like the following:

Step 1: Tag the release.
```
$ githubctl --token_file=<github token file> \
    --op=tagIstioDepsForRelease \
    --base_branch=<release branch or master>
```

Step 2: The previous command triggers rebuild and retagging on pilot, proxy, mixer and auth.
 Wait for them to finish. Check build job status [here](https://console.cloud.google.com/gcr/builds?project=istio-io&organizationId=433637338589).

Step 3: Create an update PR in istio/istio.
```
$ githubctl --token_file=<github token file> \
    --op=updateIstioVersion --base_branch=<release branch or master>
```
This will run all the presubmits on the istio repo, smoke testing the created artifacts.

Step 4: Request PR approval and wait for the PR to be merged. Note down the SHA
of the merged PR in `RELEASE_SHA`. We will create the release tag from it.

Step 5: Finalize the release. This creates the release draft in GitHub, uploads the artifacts,
 advances next release tag, and updates download script with latest release:
```
$ githubctl --token_file=<github token file> \
    --op=uploadArtifacts --base_branch=<release branch or master> \
    --next_release=<next release> --ref_sha=${RELEASE_SHA}
```

Note: 

0. If you are cutting a release off of a release branch other than master, you could have the `downloadIstioCandidate.sh` script in master branch to take on the release you just made by also specifying the flag `--update_rel_branches=master`.
1. `<next release>` is where the next release after the release draft that is being created.  For example, if you are creating 0.2.7 release, the next release could be 0.2.8.  
2. For Mac, install `gcp` via ```brew install coreutils``` and install `gtar` via ```brew install gnu-tar```.  Execute the command below instead:

```
$ TAR=gtar CP=gcp githubctl --token_file=<github token file> \
    --op=uploadArtifacts --base_branch=<release branch or master> \
    --next_release=<next release> --ref_sha=${RELEASE_SHA}
```
 
Step 6: Generating release note. This tool helps you to collect release-note left in PR descriptions. Before doing this step, make sure you already finalized and published the release, meaning it shouldn't be "draft" status and there is the version tag in release repos. This tool will return error if there is not version tag being created.

Checkout and build the tool

```Bash
$ git clone https://github.com/istio/test-infra
$ cd test-infra
$ bazel build //toolbox/release_note_collector:release_note_collector
```

If you want to get this kind of release-note between 0.2.4 and 0.2.6 from master, run the following command:
```Bash
$ bazel-bin/toolbox/release_note_collector/release_note_collector --previous_release 0.2.4 --current_release 0.2.6 --repos istio,mixer,pilot --pr_link
$ cat release-note
```
If you are doing a patch release on a release branch, you need to specify a release branch (default is `master`)
```Bash
$ bazel-bin/toolbox/release_note_collector/release_note_collector --previous_release 0.2.7 --current_release 0.2.9 --repos istio,mixer,pilot --pr_link --branch release-0.2
$ cat release-note
```
**You cannot specify a range acrossing different branch.**

Go to Istio release [page](https://github.com/istio/istio/releases) to find your
release. Click on the RELEASE_NOTES link and add your release notes. 

### Revert a failed release

When a release failed, we need to clean up partial state before retry. A common case is that a build failed when doing Step 2 from the above. We need to rollback the Step 1 by doing the following:

1. Remove new tags on the repos by finding the release and click "delete tag".
   * https://github.com/istio/auth/releases
   * https://github.com/istio/mixer/releases
   * https://github.com/istio/pilot/releases
   * https://github.com/istio/proxy/releases
1. Proceed with the above release process step [1-5].

## Manual release process (DEPRECATED)

### Creating tags

From [istio/istio](https://github.com/istio/istio), the ```istio.VERSION``` file should look like this

        $ cat istio.VERSION
        # DO NOT EDIT THIS FILE MANUALLY instead use
        # tests/updateVersion.sh (see tests/README.md)
        export CA_HUB="docker.io/istio"
        export CA_TAG="0.1.2-d773c15"
        export MIXER_HUB="docker.io/istio"
        export MIXER_TAG="0.1.2-6bfa390"
        export ISTIOCTL_URL="https://storage.googleapis.com/istio-artifacts/pilot/stable-6dbd19d/artifacts/istioctl"
        export PILOT_HUB="docker.io/istio"
        export PILOT_TAG="0.1.2-6dbd19d"

Please make sure that ISTIOCTL_URL and PILOT_TAG points to the same SHA.

The next release version is stored in ```istio.RELEASE```:

        RELEASE_TAG="$(cat istio.RELEASE)"; echo $RELEASE_TAG

The next step is to create an annotated tag for each of the repo.
Fortunately each tag above contains the short SHA at which it was built.

        PILOT_SHA=6dbd19d
        MIXER_SHA=6bfa390
        AUTH_SHA=d773c15

        $ git clone https://github.com/istio/pilot
        $ cd pilot
        $ git tag -a ${RELEASE_TAG} -m "Istio Release ${RELEASE_TAG}" ${PILOT_SHA}
        $ git push --tags origin

        $ git clone https://github.com/istio/mixer
        $ cd mixer
        $ git tag -a ${RELEASE_TAG} -m "Istio Release ${RELEASE_TAG}" ${MIXER_SHA}
        $ git push --tags origin

        $ git clone https://github.com/istio/auth
        $ cd auth
        $ git tag -a ${RELEASE_TAG} -m "Istio Release ${RELEASE_TAG}" ${AUTH_SHA}
        $ git push --tags origin

### Rebuild artifacts to include the tags

Go to Mixer [stable artifacts](https://testing.istio.io/view/All%20Jobs/job/mixer/job/stable-artifacts/)
job and click on ```Build with Parameters```.
Replace ```BRANCH_SPEC``` with the value of ```${RELEASE_TAG}```

Go to Pilot [stable artifacts](https://testing.istio.io/view/All%20Jobs/job/pilot/job/stable-artifacts/)
job and click on ```Build with Parameters```.
Replace ```BRANCH_SPEC``` with the value of ```${RELEASE_TAG}```

Go to Auth [stable artifacts](https://testing.istio.io/view/All%20Jobs/job/auth/job/stable-artifacts/)
job and click on ```Build with Parameters```.
Replace ```BRANCH_SPEC``` with the value of ```${RELEASE_TAG}```

### Updating ```istio.VERSION```

Now we need update the tags ```istio.VERSION``` to point to the release tag.

        $ git checkout -b ${USER}-${RELEASE_TAG} origin/${BRANCH}
        $ install/updateVersion.sh -p docker.io/istio,${RELEASE_TAG} \
           -c docker.io/istio,${RELEASE_TAG} -x docker.io/istio,${RELEASE_TAG} \
           -i https://storage.googleapis.com/istio-artifacts/pilot/${RELEASE_TAG}/artifacts/istioctl

Create a commit with name "Istio Release ${RELEASE_TAG}", and a PR.
Once tests are completed, merge the PR, and create an annotated tags

        $ git pull origin ${BRANCH}
        $ git tag -a ${RELEASE_TAG} -m "Istio Release ${RELEASE_TAG}" HEAD # assuming nothing else was committed
        $ git push --tags origin

### Creating archives

Sync your workspace at ${RELEASE_TAG}:

        $ git reset --hard ${RELEASE_TAG}
        $ git clean -xdf

Create the release archives

        $ ./release/create_release_archives.sh
        # On a Mac
        $ CP=gcp TAR=gtar ./release/create_release_archives.sh
        ...
        Archives are available in /tmp/istio.version.A59u/archives


Open the [GitHub Release page](https://github.com/istio/istio/releases),
and edit the release that points to ```${RELEASE_TAG}```. Uploads the artifacts created by the previous script.


### Finalizing the release

Create a PR, where you increment ```istio.RELEASE``` for the next
release and you update ```istio/downloadIstio.sh``` to point to ```${RELEASE_TAG}```

