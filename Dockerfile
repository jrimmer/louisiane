# The louisiane relay in a container:
#
#   docker build -t louisiane .
#   docker run -d --name louisiane -p 8788:8788 -v louisiane:/var/lib/louisiane \
#     louisiane relay --state-dir /var/lib/louisiane --name HOSTNAME --name LAN-IP
#   docker exec -it louisiane lal relay passwd --state-dir /var/lib/louisiane
#   docker exec louisiane lal invite HOST --state-dir /var/lib/louisiane
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/jrimmer/louisiane/internal/cli.Version=${VERSION}" -o /lal ./cmd/lal \
 && mkdir -p /state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /lal /usr/local/bin/lal
COPY --from=build --chown=65532:65532 /state /var/lib/louisiane
ENV PATH=/usr/local/bin
VOLUME /var/lib/louisiane
EXPOSE 8788
ENTRYPOINT ["/usr/local/bin/lal"]
CMD ["relay", "--state-dir", "/var/lib/louisiane"]
