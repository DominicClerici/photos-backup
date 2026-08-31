import { Switch } from 'react-native';

import { color } from '../theme';

/**
 * A setting that is on or off.
 *
 * The one control here that is not hand-built. Everything else in this
 * directory exists because the browser gallery had a shadcn component and the
 * phone had nothing, but a switch is not a shadcn shape being ported — it is
 * the iOS control, and a person deciding whether their photos back up on their
 * own should be looking at the switch they already know rather than at this
 * app's impression of one. What it does take from `theme.ts` is its colour, so
 * the rule that nothing outside this directory names one still holds.
 */
export function Toggle({
  value,
  onValueChange,
  disabled = false,
  label,
}: {
  value: boolean;
  onValueChange: (next: boolean) => void;
  disabled?: boolean;
  /** Read out in place of the switch's surroundings, which it cannot see. */
  label: string;
}) {
  return (
    <Switch
      value={value}
      onValueChange={onValueChange}
      disabled={disabled}
      accessibilityLabel={label}
      trackColor={{ false: color.input, true: color.primary }}
      thumbColor={color.foreground}
      ios_backgroundColor={color.input}
      style={disabled ? { opacity: 0.55 } : undefined}
    />
  );
}
