# Icegator

Manages streamer handoffs against a streaming radio station.

## Overview

Icegator simplifies the station operations around show handoffs on a streaming radio station, such as one hosted by [Azuracast](https://www.azuracast.com/).

An Icegator instance is aware of a station's DJs (people who can stream to it) as well as their assigned schedule for streaming to that station.

When a DJ's show time approaches, instead of carefully timing their outgoing stream to start exactly when their show starts, these DJs can stream to Icegator any time. Icegator acts as a schedule-aware multiplexor: It will cut over to the appropriate source only during their scheduled time.

Because of its role as a multiplexor, Icegator has some additional convenience features:

* **HTTP dashboard.** See which streamers are connected and their stream's diagnostics (e.g. bitrate, current metadata)
* **Level check.** Dashboard shows VU-meter style levels of any incoming stream, along with coarse categorization (low, high, good).
* **Auto-recording.** Icegator can be configured to automatically record incoming streams, saving the recording to cloud-backend storage (e.g. S3 or R2).
* **Configurable policy.** Handoff policy can be configured to either automatically cut off the previous DJ when the current DJ is ready, or to allow this switch to be done manually (though a button in the dashboard) once both are connected.
