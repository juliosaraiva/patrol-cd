/**
 * Cron schedules for project sync windows: five fields (minute hour day-of-month month
 * day-of-week) plus the @yearly/@monthly/@weekly/@daily/@hourly descriptors, evaluated in an
 * IANA time zone. Day matching follows the standard cron rule also used by the server: when
 * both day fields are restricted a day matches either; otherwise both must match.
 */

export interface CronSchedule {
  minutes: readonly number[];
  hours: readonly number[];
  days: ReadonlySet<number>;
  months: ReadonlySet<number>;
  weekdays: ReadonlySet<number>;
  /** Day-of-month / day-of-week given as "*" or "?" (unrestricted). */
  daysStar: boolean;
  weekdaysStar: boolean;
}

export type CronResult = { ok: true; schedule: CronSchedule } | { ok: false; error: string };

interface FieldSpec {
  name: string;
  min: number;
  max: number;
  names?: Record<string, number>;
}

const MONTH_NAMES: Record<string, number> = {
  jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12,
};
const DAY_NAMES: Record<string, number> = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };

const FIELDS: readonly FieldSpec[] = [
  { name: 'minute', min: 0, max: 59 },
  { name: 'hour', min: 0, max: 23 },
  { name: 'day of month', min: 1, max: 31 },
  { name: 'month', min: 1, max: 12, names: MONTH_NAMES },
  { name: 'day of week', min: 0, max: 6, names: DAY_NAMES },
];

const DESCRIPTORS: Record<string, string> = {
  '@yearly': '0 0 1 1 *',
  '@annually': '0 0 1 1 *',
  '@monthly': '0 0 1 * *',
  '@weekly': '0 0 * * 0',
  '@daily': '0 0 * * *',
  '@midnight': '0 0 * * *',
  '@hourly': '0 * * * *',
};

function parseValue(raw: string, spec: FieldSpec): number | null {
  const lower = raw.toLowerCase();
  if (spec.names && lower in spec.names) return spec.names[lower] ?? null;
  if (!/^\d+$/.test(raw)) return null;
  return Number(raw);
}

function parseField(text: string, spec: FieldSpec): { values: Set<number>; star: boolean } | string {
  const values = new Set<number>();
  let star = false;
  for (const part of text.split(',')) {
    if (part === '') return `empty value in the ${spec.name} field`;
    const [rangeText = '', stepText, extra] = part.split('/');
    if (extra !== undefined) return `"${part}" in the ${spec.name} field has more than one step`;
    let step = 1;
    if (stepText !== undefined) {
      if (!/^\d+$/.test(stepText) || Number(stepText) === 0) return `step "${stepText}" in the ${spec.name} field must be a positive number`;
      step = Number(stepText);
    }
    let start: number;
    let end: number;
    if (rangeText === '*' || rangeText === '?') {
      if (rangeText === '?' && spec.name !== 'day of month' && spec.name !== 'day of week') {
        return `"?" is only allowed in the day fields`;
      }
      start = spec.min;
      end = spec.max;
      if (step === 1) star = true;
    } else {
      const [lowText = '', highText, more] = rangeText.split('-');
      if (more !== undefined) return `"${rangeText}" in the ${spec.name} field is not a valid range`;
      const low = parseValue(lowText, spec);
      if (low === null) return `"${lowText}" is not a valid ${spec.name}`;
      start = low;
      if (highText !== undefined) {
        const high = parseValue(highText, spec);
        if (high === null) return `"${highText}" is not a valid ${spec.name}`;
        end = high;
      } else {
        // "5/15" means "from 5 to the maximum, every 15".
        end = stepText !== undefined ? spec.max : low;
      }
    }
    if (start < spec.min || end > spec.max) return `${spec.name} must be between ${spec.min} and ${spec.max}`;
    if (start > end) return `range ${start}-${end} in the ${spec.name} field is reversed`;
    for (let v = start; v <= end; v += step) values.add(v);
  }
  return { values, star };
}

export function parseCron(expression: string): CronResult {
  let text = expression.trim().replace(/\s+/g, ' ');
  if (text === '') return { ok: false, error: 'Enter a cron schedule, for example "0 22 * * *".' };
  if (text.startsWith('@')) {
    if (text.toLowerCase().startsWith('@every')) return { ok: false, error: '@every is not supported for sync windows; use a cron expression.' };
    const expanded = DESCRIPTORS[text.toLowerCase()];
    if (!expanded) return { ok: false, error: `Unknown descriptor "${text}".` };
    text = expanded;
  }
  const parts = text.split(' ');
  if (parts.length !== 5) {
    return { ok: false, error: `Expected 5 fields (minute hour day-of-month month day-of-week), found ${parts.length}.` };
  }
  const parsed: { values: Set<number>; star: boolean }[] = [];
  for (let i = 0; i < 5; i += 1) {
    const spec = FIELDS[i];
    const result = spec ? parseField(parts[i] ?? '', spec) : 'invalid field';
    if (typeof result === 'string') return { ok: false, error: result.charAt(0).toUpperCase() + result.slice(1) + '.' };
    parsed.push(result);
  }
  const [minutes, hours, days, months, weekdays] = parsed as [
    { values: Set<number>; star: boolean },
    { values: Set<number>; star: boolean },
    { values: Set<number>; star: boolean },
    { values: Set<number>; star: boolean },
    { values: Set<number>; star: boolean },
  ];
  return {
    ok: true,
    schedule: {
      minutes: [...minutes.values].sort((a, b) => a - b),
      hours: [...hours.values].sort((a, b) => a - b),
      days: days.values,
      months: months.values,
      weekdays: weekdays.values,
      daysStar: days.star,
      weekdaysStar: weekdays.star,
    },
  };
}

// Time zones ------------------------------------------------------------------------------------

const formatters = new Map<string, Intl.DateTimeFormat>();

function formatterFor(timeZone: string): Intl.DateTimeFormat {
  let f = formatters.get(timeZone);
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', {
      timeZone,
      hourCycle: 'h23',
      year: 'numeric',
      month: 'numeric',
      day: 'numeric',
      hour: 'numeric',
      minute: 'numeric',
      second: 'numeric',
    });
    formatters.set(timeZone, f);
  }
  return f;
}

export function isValidTimeZone(timeZone: string): boolean {
  if (timeZone === '') return true;
  try {
    formatterFor(timeZone);
    return true;
  } catch {
    return false;
  }
}

interface WallClock {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

function wallClock(instant: number, timeZone: string): WallClock {
  const parts = formatterFor(timeZone).formatToParts(new Date(instant));
  const get = (type: Intl.DateTimeFormatPartTypes) => Number(parts.find((p) => p.type === type)?.value ?? '0');
  return { year: get('year'), month: get('month'), day: get('day'), hour: get('hour'), minute: get('minute'), second: get('second') };
}

/** Offset of `timeZone` from UTC at `instant`, in milliseconds. */
function offsetAt(instant: number, timeZone: string): number {
  const w = wallClock(instant, timeZone);
  const asUTC = Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute, w.second);
  return asUTC - (instant - (instant % 1000 + 1000) % 1000);
}

/** UTC instant of a wall-clock time in `timeZone` (DST gaps resolve forward, as cron does). */
function zonedToInstant(year: number, month: number, day: number, hour: number, minute: number, timeZone: string): number {
  const guess = Date.UTC(year, month - 1, day, hour, minute);
  const first = guess - offsetAt(guess, timeZone);
  const second = guess - offsetAt(first, timeZone);
  return second;
}

function dayMatches(schedule: CronSchedule, year: number, month: number, day: number): boolean {
  if (!schedule.months.has(month)) return false;
  const weekday = new Date(Date.UTC(year, month - 1, day)).getUTCDay();
  const domMatch = schedule.days.has(day);
  const dowMatch = schedule.weekdays.has(weekday);
  if (schedule.daysStar || schedule.weekdaysStar) return domMatch && dowMatch;
  return domMatch || dowMatch;
}

const MAX_DAYS = 4 * 366 + 1;

/**
 * Up to `count` start times strictly after `after` (ms since epoch), evaluated in `timeZone`
 * (default UTC). Searches four years ahead, enough for leap-day schedules.
 */
export function nextRuns(schedule: CronSchedule, after: number, count: number, timeZone = 'UTC'): Date[] {
  const tz = timeZone === '' ? 'UTC' : timeZone;
  const runs: Date[] = [];
  const start = wallClock(after, tz);
  const cursor = new Date(Date.UTC(start.year, start.month - 1, start.day));
  for (let i = 0; i < MAX_DAYS && runs.length < count; i += 1) {
    const year = cursor.getUTCFullYear();
    const month = cursor.getUTCMonth() + 1;
    const day = cursor.getUTCDate();
    if (dayMatches(schedule, year, month, day)) {
      for (const hour of schedule.hours) {
        for (const minute of schedule.minutes) {
          const instant = zonedToInstant(year, month, day, hour, minute, tz);
          if (instant > after && (runs.length === 0 || instant > (runs[runs.length - 1]?.getTime() ?? 0))) {
            runs.push(new Date(instant));
            if (runs.length >= count) return runs;
          }
        }
      }
    }
    cursor.setUTCDate(cursor.getUTCDate() + 1);
  }
  return runs;
}

export interface WindowState {
  active: boolean;
  /** End of the current window when active. */
  activeUntil?: Date;
  /** Upcoming window starts (after now, or after the current window). */
  upcoming: { start: Date; end: Date }[];
}

/**
 * Whether a window with this schedule and duration is open at `now`, plus the next starts. A
 * window is open when it started less than `durationMs` ago.
 */
export function windowState(schedule: CronSchedule, durationMs: number, now: number, timeZone = 'UTC', upcomingCount = 3): WindowState {
  const [recent] = nextRuns(schedule, now - durationMs, 1, timeZone);
  const active = recent !== undefined && recent.getTime() <= now;
  const upcoming = nextRuns(schedule, now, upcomingCount, timeZone).map((start) => ({
    start,
    end: new Date(start.getTime() + durationMs),
  }));
  return active && recent ? { active, activeUntil: new Date(recent.getTime() + durationMs), upcoming } : { active, upcoming };
}

/** Short English description of common schedules, used as help next to the input. */
export function describeCron(expression: string): string | null {
  const text = expression.trim().replace(/\s+/g, ' ');
  const lower = text.toLowerCase();
  if (DESCRIPTORS[lower]) {
    const map: Record<string, string> = {
      '@yearly': 'Once a year, at midnight on January 1',
      '@annually': 'Once a year, at midnight on January 1',
      '@monthly': 'At midnight on the first day of every month',
      '@weekly': 'At midnight every Sunday',
      '@daily': 'Every day at midnight',
      '@midnight': 'Every day at midnight',
      '@hourly': 'At the start of every hour',
    };
    return map[lower] ?? null;
  }
  const m = /^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-6](?:-[0-6])?)$/.exec(text);
  if (m) {
    const [, minute = '0', hour = '0', dow = '*'] = m;
    const time = `${hour.padStart(2, '0')}:${minute.padStart(2, '0')}`;
    if (dow === '*') return `Every day at ${time}`;
    if (dow === '1-5') return `Monday to Friday at ${time}`;
    if (dow === '0-6') return `Every day at ${time}`;
  }
  return null;
}
