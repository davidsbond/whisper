FROM gcr.io/distroless/static

ARG TARGETPLATFORM

COPY $TARGETPLATFORM/whisper /usr/bin/whisper

ENTRYPOINT ["/usr/bin/whisper"]
