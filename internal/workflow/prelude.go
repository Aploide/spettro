package workflow

// prelude is evaluated before every workflow script. It defines the
// orchestration combinators — parallel, pipeline, plan, untilDry — in
// JavaScript: they are pure control flow over agent(), so implementing them in
// Go would buy nothing and cost a callback bridge. It also removes the
// non-deterministic globals.
//
// The helpers read agent, log, size and budget as globals at call time; those
// are bound right after the prelude runs.
//
// Date.now(), Math.random() and argless `new Date()` are disabled on purpose:
// a workflow must replay identically when resumed from its journal, and a
// script that stamps wall-clock time or randomises a prompt cannot. Scripts
// pass timestamps in through args and vary work by item index instead.
const prelude = `
(function () {
  'use strict';

  var RealDate = Date;
  function forbid(what, why) {
    return function () {
      throw new Error(what + ' is unavailable in workflow scripts: ' + why);
    };
  }
  Math.random = forbid('Math.random()', 'it would break resume — vary the prompt or label by index instead');
  RealDate.now = forbid('Date.now()', 'it would break resume — pass timestamps in via args');
  globalThis.Date = new Proxy(RealDate, {
    construct: function (target, argsList, newTarget) {
      if (argsList.length === 0) {
        throw new Error('new Date() is unavailable in workflow scripts: it would break resume — pass timestamps in via args');
      }
      return Reflect.construct(target, argsList, newTarget);
    },
    apply: function (target, thisArg, argsList) {
      if (argsList.length === 0) {
        throw new Error('Date() is unavailable in workflow scripts: it would break resume — pass timestamps in via args');
      }
      return Reflect.apply(target, thisArg, argsList);
    }
  });

  // canonicalKey is untilDry's default dedupe key: JSON with every object's
  // keys sorted, recursively. Plain JSON.stringify follows insertion order, so
  // two copies of one finding built in a different order — two agents
  // answering the same schema — would count as two findings and the loop
  // would never go dry. The builtins are captured here so a script that
  // reassigns JSON or Object cannot change how items are keyed.
  var stringify = JSON.stringify;
  var objectKeys = Object.keys;
  var isArray = Array.isArray;
  function canonicalKey(item) {
    return stringify(item, function (k, v) {
      if (v === null || typeof v !== 'object' || isArray(v)) return v;
      var sorted = {};
      objectKeys(v).sort().forEach(function (key) { sorted[key] = v[key]; });
      return sorted;
    });
  }

  function checkItems(kind, items) {
    if (!Array.isArray(items)) {
      throw new TypeError(kind + '() takes an array as its first argument');
    }
    if (items.length > __wfMaxItems) {
      throw new RangeError(kind + '() got ' + items.length + ' items; the limit is ' + __wfMaxItems);
    }
  }

  // parallel(thunks) — run every thunk concurrently and wait for all of them.
  // A thunk that throws (or whose agent fails) resolves to null rather than
  // rejecting the whole call, so one dead branch never loses the other
  // results; callers filter with .filter(Boolean).
  globalThis.parallel = function parallel(thunks) {
    checkItems('parallel', thunks);
    return Promise.all(thunks.map(function (thunk, i) {
      try {
        var value = typeof thunk === 'function' ? thunk(i) : thunk;
        return Promise.resolve(value).catch(function (err) {
          dropped('parallel(): item ' + i, err);
          return null;
        });
      } catch (err) {
        dropped('parallel(): item ' + i, err);
        return Promise.resolve(null);
      }
    }));
  };

  // dropped reports a branch that threw and was turned into null. Dropping
  // it keeps the other results, which is the point — but doing it silently
  // left the orchestrating model staring at a null with no idea which line of
  // its script threw, and live runs rewrote whole scripts to find out. A
  // failed agent() resolves to null without throwing, so only script errors
  // land here.
  function dropped(where, err) {
    var msg = err && err.message ? String(err.message) : String(err);
    if (err && err.name && msg.indexOf(err.name) !== 0) {
      msg = err.name + ': ' + msg;
    }
    var at = err && err.stack ? String(err.stack).split('\n').filter(function (l) {
      return l.indexOf('.workflow.js:') >= 0;
    })[0] : '';
    if (at) {
      msg += ' (' + at.trim().replace(/^at\s+/, '') + ')';
    }
    if (msg.length > 300) {
      msg = msg.slice(0, 300) + '…';
    }
    log(where + ' dropped to null: ' + msg);
  }

  // pipeline(items, ...stages) — push each item through every stage
  // independently. There is no barrier between stages: item A can be in stage
  // 3 while item B is still in stage 1, so the wall clock is the slowest
  // single chain rather than the sum of the slowest stage in each round.
  // Every stage receives (prevResult, originalItem, index). A stage that
  // throws drops that item to null and skips its remaining stages.
  globalThis.pipeline = function pipeline(items) {
    checkItems('pipeline', items);
    var stages = Array.prototype.slice.call(arguments, 1);
    return Promise.all(items.map(function (item, i) {
      var chain = Promise.resolve(item);
      var at = 0;
      stages.forEach(function (stage, s) {
        chain = chain.then(function (prev) { at = s; return stage(prev, item, i); });
      });
      return chain.catch(function (err) {
        dropped('pipeline(): item ' + i + ' at stage ' + (at + 1), err);
        return null;
      });
    }));
  };

  var PLAN_SCHEMA = {
    type: 'object',
    properties: {
      tasks: {
        type: 'array',
        items: {
          type: 'object',
          properties: {
            label: { type: 'string' },
            prompt: { type: 'string' },
            phase: { type: 'string' }
          },
          required: ['label', 'prompt']
        }
      }
    },
    required: ['tasks']
  };

  // plan(prompt, opts?) — ask one agent for a work-list and return it as
  // [{label, prompt, phase?, data?}]. This is how a script discovers its
  // fan-out at runtime instead of hardcoding a list that goes stale.
  //
  // The list is capped at opts.max (default size.fanout), and a cap that bites
  // is logged: a silently truncated work-list reads to the orchestrator as
  // "that was everything". A failed planner yields [] — the caller decides
  // whether an empty plan is an error.
  globalThis.plan = async function plan(prompt, opts) {
    opts = opts || {};
    var agentOpts = { schema: PLAN_SCHEMA, label: opts.label || 'plan' };
    ['phase', 'agentType', 'model', 'effort'].forEach(function (k) {
      if (opts[k] !== undefined && opts[k] !== null) agentOpts[k] = opts[k];
    });
    var max = typeof opts.max === 'number' && opts.max > 0 ? Math.floor(opts.max) : size.fanout;
    var out = await agent(String(prompt) +
      '\n\nReturn the work-list as {"tasks": [{"label", "prompt", "phase"?}]}: one task per independent unit of work, ' +
      'a short label, and a prompt complete enough for an agent that has seen nothing else.', agentOpts);
    if (!out || !Array.isArray(out.tasks)) return [];
    var tasks = out.tasks.filter(function (t) {
      return t && typeof t.label === 'string' && typeof t.prompt === 'string' && t.prompt.trim() !== '';
    }).map(function (t) {
      var task = { label: t.label, prompt: t.prompt };
      if (typeof t.phase === 'string' && t.phase !== '') task.phase = t.phase;
      if (t.data !== undefined) task.data = t.data;
      return task;
    });
    if (tasks.length > max) {
      log('plan(): kept ' + max + ' of ' + tasks.length + ' tasks (' + (tasks.length - max) +
        ' dropped by the cap of ' + max + ' — pass {max} to widen it)');
      tasks = tasks.slice(0, max);
    }
    return tasks;
  };

  // untilDry(round, opts?) — loop-until-dry. round(roundIndex, seen) returns
  // (a promise of) an array of items; items are deduped with opts.key
  // (default: JSON with sorted keys) against everything seen so far. The loop stops
  // after opts.dry (default 2) consecutive rounds that found nothing new,
  // after opts.maxRounds (default 8) rounds, or once the token budget is
  // spent, and returns every fresh item in discovery order. Each round, and
  // why the loop stopped, is logged.
  globalThis.untilDry = async function untilDry(round, opts) {
    if (typeof round !== 'function') {
      throw new TypeError('untilDry() takes a round function: (roundIndex, seen) => items');
    }
    opts = opts || {};
    var keyOf = typeof opts.key === 'function' ? opts.key : canonicalKey;
    var dry = typeof opts.dry === 'number' && opts.dry > 0 ? Math.floor(opts.dry) : 2;
    var maxRounds = typeof opts.maxRounds === 'number' && opts.maxRounds > 0 ? Math.floor(opts.maxRounds) : 8;
    var seen = new Set();
    var fresh = [];
    var dryStreak = 0;
    for (var i = 0; i < maxRounds; i++) {
      if (budget.remaining() <= 0) {
        log('untilDry(): stopped before round ' + (i + 1) + ': the token budget is spent');
        return fresh;
      }
      var items = await round(i, fresh.slice());
      if (items === undefined || items === null) items = [];
      if (!Array.isArray(items)) {
        throw new TypeError('untilDry(): round ' + (i + 1) + ' returned a ' + typeof items + ', not an array');
      }
      var added = 0;
      items.forEach(function (item) {
        if (item === undefined || item === null) return;
        var key = keyOf(item);
        if (typeof key !== 'string') key = canonicalKey(key);
        if (seen.has(key)) return;
        seen.add(key);
        fresh.push(item);
        added++;
      });
      dryStreak = added === 0 ? dryStreak + 1 : 0;
      log('untilDry(): round ' + (i + 1) + ': ' + added + ' new of ' + items.length + ' (' + fresh.length + ' total)');
      if (dryStreak >= dry) {
        log('untilDry(): dry after ' + (i + 1) + ' rounds');
        return fresh;
      }
    }
    log('untilDry(): stopped at the cap of ' + maxRounds + ' rounds before going dry — pass {maxRounds} to go further');
    return fresh;
  };
})();
`
