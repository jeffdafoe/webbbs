<?php

namespace App\Door\ZChat\Plugin\Trivia\Entity\Enum;

enum GameStatus: string
{
    case WAITING = 'waiting';
    case IN_PROGRESS = 'in_progress';
    case BETWEEN_ROUNDS = 'between_rounds';
    case FINISHED = 'finished';
}
