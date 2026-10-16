-- Health of ops.example.com/BackupSchedule.
--   Progressing  no status yet, or the controller has not observed the latest generation
--   Suspended    spec.suspend is true
--   Degraded     Ready=False, or the last backup failed
--   Healthy      Ready=True
local hs = { status = "Progressing", message = "Waiting for the backup controller" }

if obj.status == nil then
  return hs
end

local generation = obj.metadata.generation or 0
if obj.status.observedGeneration == nil or obj.status.observedGeneration < generation then
  hs.message = "Waiting for the backup controller to observe the latest spec"
  return hs
end

if obj.spec ~= nil and obj.spec.suspend == true then
  return { status = "Suspended", message = "Backups are suspended" }
end

local ready = nil
if obj.status.conditions ~= nil then
  for _, condition in ipairs(obj.status.conditions) do
    if condition.type == "Ready" then
      ready = condition
    end
  end
end

if ready ~= nil and ready.status == "False" then
  return { status = "Degraded", message = ready.message or "Backup schedule is not ready" }
end

if obj.status.lastBackupResult == "Failed" then
  return { status = "Degraded", message = "Last backup failed at " .. (obj.status.lastBackupTime or "an unknown time") }
end

if ready ~= nil and ready.status == "True" then
  return { status = "Healthy", message = ready.message or "Backup schedule is ready" }
end

return hs
