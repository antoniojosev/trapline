/// Sends events through the official Dart SDK and checks what arrived.
///
/// It uses the real SDK, unmodified, configured with nothing but a DSN — which
/// is the exact claim being tested. Anything this program has to work around
/// is an incompatibility, and it says so rather than adapting.
///
/// Dart earns its own suite because its stack traces are unlike anything else
/// in the matrix. Frames name `package:` and `dart:` URIs rather than file
/// paths, so the path normalisation that was written for filesystems has to
/// hold for a scheme it never saw. And an error thrown after an `await`
/// arrives with an `<asynchronous suspension>` marker where a frame would be —
/// a frame with no function and no file, which the server has to survive.
///
/// This is the pure Dart package, not the Flutter one. `sentry_flutter` wraps
/// it and shares its transport and its event shape, so the bytes on the wire
/// are the same; adding an emulator to the matrix would cost a great deal and
/// test nothing new here.
library;

import 'dart:convert';
import 'dart:io';

import 'package:sentry/sentry.dart';

const release = 'compat@1.0.0';
const environment = 'compat-test';

/// Three occurrences of one error, one of another, one thrown after an await,
/// one message, one FormatException raised by the runtime, and two errors that
/// share Dart's single anonymous exception type — seven issues if grouping is
/// right, and a different number for every way it can be wrong.
const expectedIssues = 7;
const groupedOccurrences = 3;

/// A failure of the compatibility claim, not of this program.
class CompatError implements Exception {
  CompatError(this.message);

  final String message;

  @override
  String toString() => message;
}

class PaymentDeclined implements Exception {
  PaymentDeclined(this.message);

  final String message;

  // Deliberately not prefixed with the class name. The SDK sends `toString()`
  // as the exception's value and the class name as its type, so a `toString()`
  // that repeats the type produces a title that says it twice — which is what
  // Dart's own `Exception('...')` does, and what the encoding pair below
  // documents.
  @override
  String toString() => message;
}

class UpstreamUnavailable implements Exception {
  UpstreamUnavailable(this.message);

  final String message;

  @override
  String toString() => message;
}

/// Raised one frame below the caller so the culprit has to come from the
/// stacktrace: a culprit derived from anything else would name `sendEvents`.
void charge(int orderId) {
  throw PaymentDeclined('payment declined for order $orderId');
}

void callUpstream() {
  throw UpstreamUnavailable('upstream timed out');
}

/// A throw after an await. The runtime unwinds the synchronous call stack at
/// the first suspension point and records the gap as `<asynchronous
/// suspension>`, which the SDK turns into a frame with no function and no
/// file. If grouping ever came to depend on every frame carrying a location,
/// this is the event that would prove it.
Future<void> runNightlyWorker() async {
  await Future<void>.delayed(const Duration(milliseconds: 1));
  throw StateError('the nightly worker crashed');
}

/// The one that hurts. `Exception('...')` is the idiomatic way to raise an
/// error in Dart and every one of them is the same private class, so the type
/// on the wire is identical for two completely unrelated bugs. Raised from one
/// function they also share their frames, and Dart's are especially uniform
/// because the file is a `package:` URI rather than a path. Nothing but the
/// message can tell these apart — the same shape the Go SDK exposed with
/// `errors.New`, in an ecosystem that reaches for it far more often.
void decodeBody(String encoding) {
  throw Exception('unsupported encoding $encoding');
}

/// The runtime's own exception, with a message it wrote itself.
int parseAmount(String raw) => int.parse(raw);

Future<void> main(List<String> arguments) async {
  final options = parseArguments(arguments);
  if (options['dsn']!.isEmpty ||
      options['api']!.isEmpty ||
      options['token']!.isEmpty) {
    stderr.writeln(
      'usage: main.dart -dsn ... -api ... -token ... [-project N]',
    );
    exit(2);
  }

  try {
    await sendEvents(options['dsn']!);
    final issues = await readIssues(options);
    await checkStoredEvent(options, issues);
    check(issues);
  } on CompatError catch (failure) {
    stderr.writeln('FAIL ${failure.message}');
    exit(1);
  }

  stdout.writeln(
    'ok   the official Dart SDK works against this server, DSN only',
  );
  exit(0);
}

Future<void> sendEvents(String dsn) async {
  // The whole configuration. If anything else were needed here, the
  // compatibility claim would be false.
  await Sentry.init((options) {
    options.dsn = dsn;
    options.release = release;
    options.environment = environment;
  });

  Sentry.configureScope((scope) => scope.setTag('suite', 'compat-dart'));

  // The same error three times with a value that differs each time, so this
  // exercises grouping rather than counting.
  for (var attempt = 0; attempt < groupedOccurrences; attempt++) {
    await capture(() => charge(4800 + attempt));
  }
  // A genuinely different error, which must land in its own issue.
  await capture(callUpstream);
  // A throw after an await, which arrives with an asynchronous gap in it.
  await capture(runNightlyWorker);
  // A message with no exception at all, which takes a different path through
  // grouping: no type, no frames, nothing but the text.
  await Sentry.captureMessage('cache warm-up skipped');
  // The runtime's own exception, message and all.
  await capture(() => parseAmount('not a number'));
  // Two unrelated bugs sharing Dart's one anonymous exception type.
  await capture(() => decodeBody('utf8'));
  await capture(() => decodeBody('base64'));

  await Sentry.close();
}

Future<void> capture(Function() throwing) async {
  try {
    final result = throwing();
    if (result is Future) {
      await result;
    }
  } catch (error, stackTrace) {
    await Sentry.captureException(error, stackTrace: stackTrace);
  }
}

/// check reads the issue list, most specific assertion first.
///
/// The total count is checked last on purpose. It is the assertion that fails
/// for every possible reason, so putting it first would report "got 6, want 7"
/// for a mis-grouping that the assertions below name exactly.
void check(List<Map<String, dynamic>> issues) {
  final grouped = issues
      .where((issue) => issue['times'] == groupedOccurrences)
      .toList();
  if (grouped.length != 1) {
    throw CompatError(
      'no issue collected the three occurrences of one error:${render(issues)}',
    );
  }

  if (grouped.first['last_release'] != release) {
    throw CompatError(
      'the release did not survive: ${jsonEncode(grouped.first['last_release'])}',
    );
  }
  // Not merely non-empty: the culprit has to name the function that threw, or
  // it was derived from something other than the stacktrace and would still
  // look right while pointing at the wrong place.
  final culprit = grouped.first['culprit'] as String;
  if (!culprit.endsWith('charge')) {
    throw CompatError(
      'the culprit was not derived from the stacktrace: ${jsonEncode(culprit)}',
    );
  }

  requireOwnIssue(
    issues,
    'a second, unrelated exception',
    (title) => title.contains('upstream timed out'),
  );
  requireOwnIssue(
    issues,
    'a message with no exception',
    (title) => title == 'cache warm-up skipped',
  );

  // The async throw must produce a culprit too. Its frames are interrupted by
  // an asynchronous gap, so a server that assumed every frame carries a
  // location would come up empty here and nowhere else.
  final asynchronous = requireOwnIssue(
    issues,
    'a throw after an await',
    (title) => title.contains('the nightly worker crashed'),
  );
  final asyncCulprit = asynchronous['culprit'] as String;
  if (!asyncCulprit.contains('runNightlyWorker')) {
    throw CompatError(
      'no culprit was derived from the async frames: ${jsonEncode(asyncCulprit)}',
    );
  }

  // The runtime's own message has to survive normalisation intact, or every
  // parse failure in an application becomes one issue.
  requireOwnIssue(
    issues,
    "the runtime's own FormatException",
    (title) => title.contains('FormatException'),
  );

  checkEncodingPair(issues);

  if (issues.length != expectedIssues) {
    throw CompatError(
      'got ${issues.length} issues, want $expectedIssues:${render(issues)}',
    );
  }
}

/// checkEncodingPair asserts that two unrelated errors stayed apart.
///
/// `unsupported encoding utf8` and `unsupported encoding base64` are two
/// different bugs. In Dart they share more than a stacktrace: `Exception()` is
/// a factory for one private class, so both arrive with the same type as well.
/// The normalised message is the only thing that can separate them, and both
/// tokens are a letter followed by a digit — the shape a normaliser once
/// treated as a machine name and erased, merging unrelated bugs and deleting
/// the second title from the product entirely.
void checkEncodingPair(List<Map<String, dynamic>> issues) {
  for (final encoding in ['utf8', 'base64']) {
    requireOwnIssue(
      issues,
      'the error about encoding $encoding',
      (title) => title.contains('unsupported encoding $encoding'),
    );
  }
}

Map<String, dynamic> requireOwnIssue(
  List<Map<String, dynamic>> issues,
  String description,
  bool Function(String title) matches,
) {
  final matching = issues
      .where((issue) => matches(issue['title'] as String))
      .toList();
  if (matching.length != 1) {
    throw CompatError(
      '$description did not land in exactly one issue of its own:${render(issues)}',
    );
  }
  if (matching.first['times'] != 1) {
    throw CompatError(
      '$description was counted ${matching.first['times']} times, want 1:${render(issues)}',
    );
  }
  return matching.first;
}

/// checkStoredEvent looks at the payload the server kept, which is the only
/// place the frame shape is visible. The issue list shows what grouping
/// decided; this shows what it decided it from.
Future<void> checkStoredEvent(
  Map<String, String> options,
  List<Map<String, dynamic>> issues,
) async {
  final grouped = issues
      .where((issue) => issue['times'] == groupedOccurrences)
      .toList();
  if (grouped.isEmpty) {
    // check() reports this with more context; nothing to inspect here.
    return;
  }

  final detail = await getJSON(
    options,
    '/api/v1/projects/${options['project']}/issues/${grouped.first['id']}',
  );
  final events = detail['events'] as List<dynamic>;
  if (events.isEmpty) {
    throw CompatError('the issue kept no event payload');
  }
  final event = events.first as Map<String, dynamic>;
  if (event['environment'] != environment) {
    throw CompatError(
      'the environment did not survive: ${jsonEncode(event['environment'])}',
    );
  }

  final payload = event['payload'] as Map<String, dynamic>;
  final values =
      ((payload['exception'] as Map<String, dynamic>?)?['values']
          as List<dynamic>?) ??
      [];
  if (values.isEmpty) {
    throw CompatError('the stored event carries no exception');
  }
  final last = values.last as Map<String, dynamic>;
  final frames =
      ((last['stacktrace'] as Map<String, dynamic>?)?['frames']
          as List<dynamic>?) ??
      [];
  if (frames.isEmpty) {
    throw CompatError('the stored event carries no stack frames');
  }

  // Without in-app frames grouping falls back to the whole stack, and the
  // runtime's own `dart:` frames are identical for every error raised the same
  // way — unrelated bugs in one program would become one issue.
  final inApp = frames
      .where((frame) => (frame as Map<String, dynamic>)['in_app'] == true)
      .toList();
  if (inApp.isEmpty) {
    throw CompatError(
      'no frame was marked in_app, so grouping fell back to the whole stack',
    );
  }

  await checkAsynchronousGap(options, issues);

  // Dart names files with URIs, not paths: the program's own frames say
  // `file:///app/bin/main.dart` and the SDK's say `package:sentry/...`.
  // Grouping and the culprit both depend on there being something there, so
  // the assertion is on a location being present rather than on which field
  // carried it.
  for (final frame in inApp) {
    final typed = frame as Map<String, dynamic>;
    final absPath = (typed['abs_path'] as String?) ?? '';
    final filename = (typed['filename'] as String?) ?? '';
    if (absPath.isEmpty && filename.isEmpty) {
      throw CompatError(
        'an in-app frame arrived with no path at all: ${jsonEncode(typed)}',
      );
    }
  }
}

/// checkAsynchronousGap asserts that a stacktrace with a hole in it survives.
///
/// Dart records the boundary between an async caller and its callee as a frame
/// that is not a frame: no function, no file, and `<asynchronous suspension>`
/// where a path would be. Nothing else in the matrix sends one. A server that
/// assumed every frame carries a location would either lose the stacktrace or
/// group every asynchronous error in a program together, and it would do so
/// only for Dart and Flutter — the two clients least likely to be the ones
/// anybody tested with.
Future<void> checkAsynchronousGap(
  Map<String, String> options,
  List<Map<String, dynamic>> issues,
) async {
  final asynchronous = issues
      .where(
        (issue) =>
            (issue['title'] as String).contains('the nightly worker crashed'),
      )
      .toList();
  if (asynchronous.isEmpty) {
    return; // check() reports this with more context.
  }

  final detail = await getJSON(
    options,
    '/api/v1/projects/${options['project']}/issues/${asynchronous.first['id']}',
  );
  final payload =
      (detail['events'] as List<dynamic>).first as Map<String, dynamic>;
  final values =
      (((payload['payload'] as Map<String, dynamic>)['exception']
              as Map<String, dynamic>?)?['values']
          as List<dynamic>?) ??
      [];
  final frames =
      (((values.last as Map<String, dynamic>)['stacktrace']
              as Map<String, dynamic>?)?['frames']
          as List<dynamic>?) ??
      [];

  final gaps = frames.where((frame) {
    final typed = frame as Map<String, dynamic>;
    return ((typed['function'] as String?) ?? '').isEmpty &&
        ((typed['filename'] as String?) ?? '').isEmpty;
  }).toList();
  if (gaps.isEmpty) {
    throw CompatError(
      'a throw after an await arrived with no asynchronous gap in its stacktrace, so this '
      'assertion is no longer testing what it was written for: ${jsonEncode(frames)}',
    );
  }
  if ((asynchronous.first['culprit'] as String).isEmpty) {
    throw CompatError(
      'a stacktrace containing an asynchronous gap produced no culprit at all',
    );
  }
}

Future<List<Map<String, dynamic>>> readIssues(
  Map<String, String> options,
) async {
  // The listing is a page, not a bare array: it carries the status counts and
  // a cursor alongside the issues.
  final page = await getJSON(
    options,
    '/api/v1/projects/${options['project']}/issues',
  );
  return (page['issues'] as List<dynamic>).cast<Map<String, dynamic>>();
}

Future<Map<String, dynamic>> getJSON(
  Map<String, String> options,
  String path,
) async {
  final client = HttpClient();
  try {
    final request = await client.getUrl(Uri.parse('${options['api']}$path'));
    request.headers.set('Authorization', 'Bearer ${options['token']}');
    request.headers.set('X-Trapline-Request', '1');
    final response = await request.close();
    final body = await response.transform(utf8.decoder).join();
    if (response.statusCode != 200) {
      throw CompatError('reading $path: status ${response.statusCode}: $body');
    }
    return jsonDecode(body) as Map<String, dynamic>;
  } finally {
    client.close();
  }
}

/// parseArguments accepts the single-dash spelling as well as the double-dash
/// one. Go's flag package treats them as the same thing, so the runner passes
/// `-dsn` to every suite and a Dart suite that only understood `--dsn` would
/// look broken for a reason that has nothing to do with the SDK.
Map<String, String> parseArguments(List<String> arguments) {
  final options = {'dsn': '', 'api': '', 'token': '', 'project': '1'};
  for (var index = 0; index < arguments.length; index++) {
    var name = arguments[index].replaceFirst(RegExp(r'^--?'), '');
    String? inline;
    final separator = name.indexOf('=');
    if (separator >= 0) {
      inline = name.substring(separator + 1);
      name = name.substring(0, separator);
    }
    if (!options.containsKey(name)) {
      continue;
    }
    options[name] =
        inline ?? (index + 1 < arguments.length ? arguments[++index] : '');
  }
  return options;
}

String render(List<Map<String, dynamic>> issues) {
  final rows = issues
      .map(
        (issue) => {
          'title': issue['title'],
          'culprit': issue['culprit'],
          'level': issue['level'],
          'times': issue['times'],
          'last_release': issue['last_release'],
        },
      )
      .toList();
  return '\n  ${const JsonEncoder.withIndent('  ').convert(rows)}';
}
