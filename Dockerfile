FROM docker.io/library/golang:latest AS builder
RUN git config --global --add safe.directory '*'

ARG TARGETPLATFORM
ARG MODE

WORKDIR /src
COPY . /src
RUN bash /src/build/docker/build-binary-file.sh ${MODE} ${TARGETPLATFORM} 

FROM --platform=${TARGETPLATFORM} alpine:latest

WORKDIR /opt/mieru
COPY --from=builder /release /opt/mieru

RUN mkdir /config
VOLUME [ "/config" ]

ENTRYPOINT [ "/opt/mieru/entrypoint.sh" ]

