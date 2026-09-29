#!/bin/sh
# Prints a JSON status document and exits.
echo "{\"hostname\": \"$(hostname)\", \"pid\": $$, \"alpine\": \"$(cat /etc/alpine-release)\"}" 
