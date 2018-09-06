# emarsys_exporter

This exporter collects Emarsys metrics.

## Building and running

    go get github.com/runtastic/emarsys_exporter

For generating a token to authenticate at Emarsys you have to set your Emarsys username and password in your environment:

    export EMARSYSUSER=xxxxxx
    export EMARSYSPASS=xxxxxx

    ${GOPATH-$HOME/go}/bin/emarsys_exporter
