-- Parameters arrive as strings in actionParams.
local days = tonumber(actionParams["days"])
if days == nil or days ~= math.floor(days) or days < 1 or days > 3650 then
  error("days must be an integer between 1 and 3650", 0)
end
obj.spec.retentionDays = days
return obj
