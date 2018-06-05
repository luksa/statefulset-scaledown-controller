FROM busybox
ADD controller /controller

ENTRYPOINT ["/controller", "--alsologtostderr", "--v", "3"]
