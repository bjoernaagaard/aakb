FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /aakb ./cmd/aakb
RUN mkdir -p /data/cache /data/downloads

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /aakb /aakb
COPY --from=build --chown=65534:65534 /data /data
ENV AAKB_CACHE_DIR=/data/cache AAKB_ARCHIVE_DOWNLOAD_PATH=/data/downloads
VOLUME /data
USER 65534:65534
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/aakb", "health"]
ENTRYPOINT ["/aakb"]
CMD ["serve", "-addr", "0.0.0.0:8080"]
