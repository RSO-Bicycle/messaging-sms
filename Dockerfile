FROM scratch
WORKDIR /app
ADD ./bin/main /app
ADD ./config.yaml.example /app/config.yaml
CMD ["/app/main"]