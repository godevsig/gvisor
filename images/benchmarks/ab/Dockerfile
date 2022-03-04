FROM ubuntu:18.04

ENV DEBIAN_FRONTEND="noninteractive"
RUN set -x \
        && apt-get update \
        && apt-get install -y \
            apache2-utils \
        && rm -rf /var/lib/apt/lists/*
