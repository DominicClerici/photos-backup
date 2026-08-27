import { SearchScreen } from '../src/search';

/**
 * Asking the archive a question.
 *
 * A root route rather than a tab, and for the browser's reason: a search is a
 * question rather than a destination — you ask one, read the answer and leave.
 * Which also makes the Back gesture do the two things it should here, and the
 * whole reason the request lives in the route's parameters: back out of a
 * search and you are where you were, back out of a chip you removed by mistake
 * and the reading it came from is on screen again.
 */
export default function SearchRoute() {
  return <SearchScreen />;
}
