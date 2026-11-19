# Specification

## What it is

A calendar that tells its user, at any moment, the **one thing to do right now**, and keeps that answer right as the day changes. It is built for someone with ADHD: planning, re-planning, estimating and remembering are the system's job, not the user's.

It has two parts:

- **Calendar:** CalDAV for calendar apps, plus a web UI. Built.
- **Coach:** the planning layer, which is planned. It turns the calendar into a live plan, mostly driven by Claude.

## Built

- CalDAV server with a default calendar per user, behind a reverse proxy that authenticates (`X-Remote-User`).
- Web UI under `/~/` that lists, adds and deletes events.
- Web Push to every device of a user, including iPhone home-screen apps:
  - Event alarms go out as notifications.
  - Per-device status and test buttons; failures are never silent.
- `POST /~/api/notify` for external callers.

## Coach (planned)

### The one rule

- **One thing to do now:**
  - At any time there is exactly one current item: a task, a trip, an appointment, or explicit free time / rest.
  - The "now" screen shows only that item, how long it has, and how to say it's done.
  - Everything else (the day's plan, the calendar) is one tap away, never in the way.

### Planning from the actual moment

- **Plans start from real events, not the clock.**
  - Waking up, leaving or arriving somewhere, and finishing a task each start or shift the plan.
  - Example: waking at 07:40 instead of 07:00 moves the whole morning routine, not just its first item.
- **Routines:** named, ordered lists of steps with typical durations. For example, the morning is make tea, breakfast, shower, get dressed, pack bag, leave. A signal (woke up) starts a routine, which is then laid out from that moment.
- **Fixed appointments:**
  - Work and doctor's appointments are hard constraints.
  - Everything flexible is laid out around them.
  - Each appointment with a location gets a "leave by" time: travel time + tolerance + a short "get ready to leave" step.
- **Overflow:** when not everything fits, optional items are dropped or moved to later, lowest priority first. The leave-by time for a fixed appointment is protected above everything flexible.
- **Context-aware decisions** are made at the moments they matter. Example, on leaving work:
  - Is there time for 30 more minutes in the park, given the weather, whether the user was already there this morning, and the evening's appointments?
  - How much time is left for everyday chores tonight (start the washer, hang up the laundry, hoover)?
- **Long-running chores** are split into start and finish. "Start the washer" schedules "hang up laundry" for when the cycle ends.

### Done before next

- A task stays current until the user confirms it: Done, Skip, or "more time".
- **At its planned end:** the user is asked once whether it's done. If not, they're nudged again a few minutes later, and the rest of the plan shifts.
- **Leave-by at risk:** if an overrun threatens a leave-by time, the user is told plainly what gets dropped ("Skip breakfast at home, leave in 10 min").
- **Signals count as confirmation:**
  - Leaving home confirms "leave for work".
  - Arriving at work ends the trip.
- **Confirming is effortless:** one tap on the now screen, the iPhone Action Button, or "Hey Siri, done".
- **Learned durations:** actual durations are recorded, and future estimates use them (leaning long rather than short).

### Changing the plan by talking

- **Brain dump:** the user tells a Claude session anything, in any order, and the plan changes accordingly. Examples:
  - "Dentist moved to Thursday 3 pm."
  - "Too tired, skip hoovering."
  - "I need to call mom this week."
- **Shared email:** sharing an email with Claude adds the appointment in it, with time, place and travel.
- **Explanations:** every change Claude makes is shown with a one-line reason, and can be undone.

### Inputs (from the iPhone, without the user thinking about it)

- **Wake-up:** the Sleep "wake up" / alarm-stopped automation.
- **Locations:** arrive at / leave home, work, the park, and other saved places.
- **Done:** Action Button or Siri shortcut.
- **Messages:** brain dumps and shared emails go through the Claude app.

### Reliability

- **No Claude in the hot path:** the "now" item, notifications and re-timing work even when Claude is slow, down, or out of quota.
  - Claude improves the plan; it is not needed to follow it.
  - When a Claude decision fails, a simple default applies, and the user is told (with an error code).
- **No silent failures,** as with Web Push today:
  - Every input, decision and notification is logged.
  - Failures carry a stable code shown in the UI.
