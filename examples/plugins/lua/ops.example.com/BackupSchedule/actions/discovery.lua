-- Actions offered for ops.example.com/BackupSchedule in the UI and API.
local actions = {}
local suspended = obj.spec ~= nil and obj.spec.suspend == true

actions["suspend"] = {
  ["disabled"] = suspended,
  ["iconClass"] = "fa fa-fw fa-pause-circle",
}
actions["resume"] = {
  ["disabled"] = not suspended,
  ["iconClass"] = "fa fa-fw fa-play-circle",
}
actions["set-retention"] = {
  ["displayName"] = "Set retention",
  ["params"] = { { ["name"] = "days" } },
}
return actions
