FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /aakb ./cmd/aakb

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /aakb /aakb
USER 65534:65534
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/aakb", "version"]
ENTRYPOINT ["/aakb"]
CMD ["serve", "-addr", "0.0.0.0:8080"]
