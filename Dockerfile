FROM btwiuse/arch:golang AS build

COPY . /src
WORKDIR /src
ENV CGO_ENABLED=0
RUN go mod tidy
RUN go build -o /bin/w9y ./cmd/w9y

FROM btwiuse/arch

WORKDIR /app
COPY --from=build /bin/w9y /bin/w9y
CMD ["w9y"]
