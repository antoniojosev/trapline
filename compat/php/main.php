<?php

/**
 * Sends events through the official PHP SDK and checks what arrived.
 *
 * It uses the real SDK, unmodified, configured with nothing but a DSN — which
 * is the exact claim being tested. Anything this program has to work around is
 * an incompatibility, and it says so rather than adapting.
 *
 * PHP earns its own suite rather than being assumed equivalent to the others
 * because two things here have no counterpart in the rest of the matrix. The
 * engine itself raises exceptions whose message embeds a file path and a line
 * number — a shape no SDK constructs and no hand-written fixture would have
 * thought of. And `previous` chains are the idiomatic way to wrap an error,
 * which puts several exceptions on the wire and makes the order of `values[]`
 * the difference between an issue titled after the failure and one titled
 * after its cause.
 */

declare(strict_types=1);

// A namespace, because every PHP application has one and the SDK reports the
// qualified class name: `Trapline\CompatPhp\PaymentDeclined` is what a real
// issue title looks like, and a bare `PaymentDeclined` would test a shape no
// deployed application produces.
namespace Trapline\CompatPhp;

require __DIR__ . '/vendor/autoload.php';

const RELEASE = 'compat@1.0.0';
const ENVIRONMENT = 'compat-test';

// Three occurrences of one error, one of another, one `previous` chain, one
// message, one TypeError raised by the engine, and two errors that differ only
// by a charset name — seven issues if grouping is right, and a different
// number for every way it can be wrong.
const EXPECTED_ISSUES = 7;
const GROUPED_OCCURRENCES = 3;

final class CompatError extends \RuntimeException
{
}

final class PaymentDeclined extends \RuntimeException
{
}

final class UpstreamUnavailable extends \RuntimeException
{
}

final class ConfigurationMissing extends \RuntimeException
{
}

/**
 * Raised one frame below the caller so the culprit has to come from the
 * stacktrace: a culprit derived from anything else would name `sendEvents`.
 */
function charge(int $orderId): void
{
    throw new PaymentDeclined(sprintf('payment declined for order %d', $orderId));
}

function callUpstream(): void
{
    throw new UpstreamUnavailable('upstream timed out');
}

/**
 * `throw ... previous:` is how PHP wraps an error, and it is the reason the
 * order of `values[]` matters. The exception that was actually raised is
 * `ConfigurationMissing`; `RuntimeException` is what it wrapped. Everything the
 * issue shows — title, culprit, grouping — has to come from the raised one.
 */
function boot(): void
{
    try {
        throw new \RuntimeException('SETTINGS_PATH is not set');
    } catch (\RuntimeException $cause) {
        throw new ConfigurationMissing('boot failed', 0, $cause);
    }
}

/**
 * The engine's own TypeError. Under strict_types PHP raises it with a message
 * that carries the qualified function name, the argument position and name,
 * both types, and the caller's absolute path and line number. Nothing else in
 * the matrix produces a message with that much structure in it, and every part
 * of it passes through the message normaliser on the way to an issue.
 */
function settleAmount(int $amount): int
{
    return $amount;
}

/**
 * Two errors from one function whose messages differ only by a charset name.
 * `utf8mb4` and `latin1` are ordinary words in this ecosystem and are the
 * entire difference between two unrelated bugs — the same shape that a
 * normaliser once erased for the Node suite.
 */
function openConnection(string $charset): void
{
    throw new \PDOException(sprintf('unsupported charset %s', $charset));
}

function main(array $argv): int
{
    $options = parseArguments($argv);
    foreach (['dsn', 'api', 'token'] as $required) {
        if ($options[$required] === '') {
            fwrite(STDERR, "usage: main.php -dsn ... -api ... -token ... [-project N]\n");
            return 2;
        }
    }

    try {
        sendEvents($options['dsn']);
        $issues = readIssues($options);
        checkStoredEvent($options, $issues);
        check($issues);
    } catch (CompatError $failure) {
        fwrite(STDERR, 'FAIL ' . $failure->getMessage() . "\n");
        return 1;
    }

    fwrite(STDOUT, "ok   the official PHP SDK works against this server, DSN only\n");

    return 0;
}

function sendEvents(string $dsn): void
{
    // The whole configuration. If anything else were needed here, the
    // compatibility claim would be false.
    \Sentry\init([
        'dsn' => $dsn,
        'release' => RELEASE,
        'environment' => ENVIRONMENT,
    ]);

    \Sentry\configureScope(static function (\Sentry\State\Scope $scope): void {
        $scope->setTag('suite', 'compat-php');
    });

    // The same error three times with a value that differs each time, so this
    // exercises grouping rather than counting.
    for ($attempt = 0; $attempt < GROUPED_OCCURRENCES; $attempt++) {
        capture(static fn () => charge(4800 + $attempt));
    }
    // A genuinely different error, which must land in its own issue.
    capture(callUpstream(...));
    // A wrapped error: two exceptions on the wire, one issue, titled after the
    // one that was raised.
    capture(boot(...));
    // A message with no exception at all, which takes a different path through
    // grouping: no type, no frames, nothing but the text.
    \Sentry\captureMessage('cache warm-up skipped');
    // The engine's own TypeError, message and all.
    capture(static fn () => settleAmount('4800'));
    // Two unrelated bugs that share a type and a call site.
    capture(static fn () => openConnection('utf8mb4'));
    capture(static fn () => openConnection('latin1'));

    $client = \Sentry\SentrySdk::getCurrentHub()->getClient();
    if ($client === null) {
        throw new CompatError('the SDK kept no client after init');
    }
    $flushed = $client->flush(10);
    if ((string) $flushed->getStatus() === (string) \Sentry\Transport\ResultStatus::failed()) {
        throw new CompatError('the SDK could not flush its events within ten seconds');
    }
}

function capture(callable $throwing): void
{
    try {
        $throwing();
    } catch (\Throwable $error) {
        \Sentry\captureException($error);
    }
}

/**
 * check reads the issue list, most specific assertion first.
 *
 * The total count is checked last on purpose. It is the assertion that fails
 * for every possible reason, so putting it first would report "got 6, want 7"
 * for a mis-grouping that the assertions below name exactly.
 */
function check(array $issues): void
{
    $grouped = null;
    foreach ($issues as $issue) {
        if ($issue['times'] === GROUPED_OCCURRENCES) {
            $grouped = $issue;
            break;
        }
    }
    if ($grouped === null) {
        throw new CompatError(
            'no issue collected the three occurrences of one error:' . render($issues)
        );
    }

    if (($grouped['last_release'] ?? '') !== RELEASE) {
        throw new CompatError('the release did not survive: ' . json_encode($grouped['last_release'] ?? null));
    }
    // Not merely non-empty: the culprit has to name the function that threw,
    // or it was derived from something other than the stacktrace and would
    // still look right while pointing at the wrong place.
    if (!str_ends_with($grouped['culprit'], 'charge')) {
        throw new CompatError('the culprit was not derived from the stacktrace: ' . json_encode($grouped['culprit']));
    }

    requireOwnIssue($issues, 'Trapline\CompatPhp\UpstreamUnavailable: upstream timed out', 'a second, unrelated exception');
    requireOwnIssue($issues, 'cache warm-up skipped', 'a message with no exception');

    // The wrapped error is titled after the exception that was raised, not
    // after the one it wrapped. Getting this backwards is invisible in a unit
    // test and obvious to anyone reading the issue list.
    $wrapped = requireOwnIssue(
        $issues,
        'Trapline\CompatPhp\ConfigurationMissing: boot failed',
        'an exception wrapping a cause'
    );
    if (!str_ends_with($wrapped['culprit'], 'boot')) {
        throw new CompatError(
            'the culprit of a wrapped exception came from the cause, not from the raised one: '
            . json_encode($wrapped['culprit'])
        );
    }

    checkEngineTypeError($issues);
    checkCharsetPair($issues);

    if (count($issues) !== EXPECTED_ISSUES) {
        throw new CompatError(sprintf('got %d issues, want %d:%s', count($issues), EXPECTED_ISSUES, render($issues)));
    }
}

/**
 * checkEngineTypeError asserts that a message the engine wrote survives.
 *
 * PHP puts the caller's absolute path and line number inside the message.
 * Both are exactly what the normaliser is built to erase, and erasing the
 * wrong part of this message would merge every argument-type mistake in a
 * codebase into one issue.
 */
function checkEngineTypeError(array $issues): void
{
    $typeErrors = array_values(array_filter(
        $issues,
        static fn (array $issue): bool => str_starts_with($issue['title'], 'TypeError: ')
    ));
    if (count($typeErrors) !== 1) {
        throw new CompatError('the engine\'s own TypeError did not land in one issue:' . render($issues));
    }
    if (!str_contains($typeErrors[0]['title'], 'must be of type int, string given')) {
        throw new CompatError('the TypeError message did not survive: ' . json_encode($typeErrors[0]['title']));
    }
    if (!str_ends_with($typeErrors[0]['culprit'], 'settleAmount')) {
        throw new CompatError(
            'the culprit of the engine\'s TypeError does not name the function that rejected the argument: '
            . json_encode($typeErrors[0]['culprit'])
        );
    }
}

function checkCharsetPair(array $issues): void
{
    foreach (['utf8mb4', 'latin1'] as $charset) {
        requireOwnIssue(
            $issues,
            'PDOException: unsupported charset ' . $charset,
            'the error about charset ' . $charset
        );
    }
}

function requireOwnIssue(array $issues, string $title, string $description): array
{
    $matching = array_values(array_filter($issues, static fn (array $issue): bool => $issue['title'] === $title));
    if (count($matching) !== 1) {
        throw new CompatError($description . ' did not land in exactly one issue of its own:' . render($issues));
    }
    if ($matching[0]['times'] !== 1) {
        throw new CompatError(sprintf('%s was counted %d times, want 1:%s', $description, $matching[0]['times'], render($issues)));
    }

    return $matching[0];
}

/**
 * checkStoredEvent looks at the payload the server kept, which is the only
 * place the exception chain and the frame shape are visible. The issue list
 * shows what grouping decided; this shows what it decided it from.
 */
function checkStoredEvent(array $options, array $issues): void
{
    $wrapped = null;
    foreach ($issues as $issue) {
        if ($issue['title'] === 'Trapline\CompatPhp\ConfigurationMissing: boot failed') {
            $wrapped = $issue;
            break;
        }
    }
    if ($wrapped === null) {
        // check() reports this with more context; nothing to inspect here.
        return;
    }

    $detail = getJSON($options, sprintf('/api/v1/projects/%s/issues/%d', $options['project'], $wrapped['id']));
    $event = $detail['events'][0] ?? null;
    if ($event === null) {
        throw new CompatError('the issue kept no event payload');
    }
    if (($event['environment'] ?? '') !== ENVIRONMENT) {
        throw new CompatError('the environment did not survive: ' . json_encode($event['environment'] ?? null));
    }

    $values = $event['payload']['exception']['values'] ?? [];
    if (count($values) !== 2) {
        throw new CompatError(sprintf('a wrapped exception arrived as %d values, want 2', count($values)));
    }
    // The protocol puts the causes first and the exception that was actually
    // raised last. An SDK that sent them the other way round would give every
    // wrapped error in a PHP application a title and a culprit describing the
    // cause instead of the failure.
    $last = $values[count($values) - 1];
    if (($last['type'] ?? '') !== 'Trapline\CompatPhp\ConfigurationMissing') {
        throw new CompatError(
            'the last entry of values[] is not the exception that was raised: '
            . json_encode(array_map(static fn (array $value): string => $value['type'] ?? '', $values))
        );
    }

    $frames = $last['stacktrace']['frames'] ?? [];
    if ($frames === []) {
        throw new CompatError('the stored event carries no stack frames');
    }
    $inApp = array_values(array_filter($frames, static fn (array $frame): bool => ($frame['in_app'] ?? false) === true));
    if ($inApp === []) {
        throw new CompatError('no frame was marked in_app, so grouping fell back to the whole stack');
    }
    foreach ($inApp as $frame) {
        if (($frame['abs_path'] ?? '') === '' && ($frame['filename'] ?? '') === '') {
            throw new CompatError('an in-app frame arrived with no path at all: ' . json_encode($frame));
        }
    }
}

function readIssues(array $options): array
{
    $page = getJSON($options, sprintf('/api/v1/projects/%s/issues', $options['project']));

    return $page['issues'] ?? [];
}

function getJSON(array $options, string $path): array
{
    $context = stream_context_create([
        'http' => [
            'method' => 'GET',
            'header' => "Authorization: Bearer {$options['token']}\r\nX-Trapline-Request: 1\r\n",
            'ignore_errors' => true,
        ],
    ]);
    $body = @file_get_contents($options['api'] . $path, false, $context);
    if ($body === false) {
        throw new CompatError('reading ' . $path . ': the request failed');
    }
    // $http_response_header is set by the stream wrapper in the local scope.
    $status = isset($http_response_header[0]) ? $http_response_header[0] : '';
    if (!str_contains($status, ' 200')) {
        throw new CompatError('reading ' . $path . ': ' . $status . ': ' . $body);
    }
    $decoded = json_decode($body, true);
    if (!is_array($decoded)) {
        throw new CompatError('decoding ' . $path . ': ' . $body);
    }

    return $decoded;
}

/**
 * parseArguments accepts the single-dash spelling as well as the double-dash
 * one. Go's flag package treats them as the same thing, so the runner passes
 * `-dsn` to every suite and a PHP suite that only understood `--dsn` would
 * look broken for a reason that has nothing to do with the SDK.
 */
function parseArguments(array $argv): array
{
    $options = ['dsn' => '', 'api' => '', 'token' => '', 'project' => '1'];
    for ($index = 1; $index < count($argv); $index++) {
        $argument = ltrim($argv[$index], '-');
        $value = null;
        if (str_contains($argument, '=')) {
            [$argument, $value] = explode('=', $argument, 2);
        }
        if (!array_key_exists($argument, $options)) {
            continue;
        }
        $options[$argument] = $value ?? ($argv[++$index] ?? '');
    }

    return $options;
}

function render(array $issues): string
{
    $rows = array_map(static fn (array $issue): array => [
        'title' => $issue['title'],
        'culprit' => $issue['culprit'],
        'level' => $issue['level'],
        'times' => $issue['times'],
        'last_release' => $issue['last_release'] ?? '',
    ], $issues);

    return "\n  " . json_encode($rows, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES);
}

exit(main($argv));
