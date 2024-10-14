FROM docker.io/library/golang:latest AS build


WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . ./

WORKDIR /app/proxy
RUN CGO_ENABLED=0 go build -o proxy -ldflags '-extldflags "-static" -w -s'  .

FROM containers.torproject.org/tpo/tpa/base-images/debian:bookworm as debian-base

# Install dependencies to add Tor's repository.
RUN apt-get update && apt-get install -y \
    curl \
    gpg \
    gpg-agent \
    ca-certificates \
    libcap2-bin \
    --no-install-recommends

# See: <https://2019.www.torproject.org/docs/debian.html.en>
RUN curl https://deb.torproject.org/torproject.org/A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89.asc | gpg --import
RUN gpg --export A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89 | apt-key add -

RUN printf "deb https://deb.torproject.org/torproject.org bookworm main\n" >> /etc/apt/sources.list.d/tor.list

# Install remaining dependencies.
RUN apt-get update && apt-get install -y \
    tor \
    tor-geoipdb \
    --no-install-recommends


FROM scratch

COPY --from=debian-base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=debian-base /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=debian-base /usr/share/tor/geoip* /usr/share/tor/
COPY --from=build /app/proxy/proxy /bin/proxy

ENTRYPOINT [ "/bin/proxy" ]

# Set some labels
# io.containers.autoupdate label will instruct podman to reach out to the
# corresponding registry to check if the image has been updated. If an image
# must be updated, Podman pulls it down and restarts the systemd unit executing
# the container. See podman-auto-update(1) for more details, or
# https://docs.podman.io/en/latest/markdown/podman-auto-update.1.html
LABEL io.containers.autoupdate=registry
LABEL org.opencontainers.image.authors="anti-censorship-team@lists.torproject.org"
